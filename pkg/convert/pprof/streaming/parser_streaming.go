package streaming

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"github.com/pyroscope-io/pyroscope/pkg/storage"
	"github.com/pyroscope-io/pyroscope/pkg/storage/metadata"
	"github.com/pyroscope-io/pyroscope/pkg/storage/segment"
	"github.com/pyroscope-io/pyroscope/pkg/storage/tree"
	"github.com/richardartoul/molecule"
	"github.com/richardartoul/molecule/src/codec"
	"github.com/valyala/bytebufferpool"
	"io"
	"time"
)

type ParserConfig struct {
	Putter        storage.Putter
	SpyName       string
	Labels        map[string]string
	SkipExemplars bool
	SampleTypes   map[string]*tree.SampleTypeConfig
	//StackFrameFormatter StackFrameFormatter
}

type MoleculeParser struct {
	putter        storage.Putter
	spyName       string
	labels        map[string]string
	skipExemplars bool
	sampleTypes   map[string]*tree.SampleTypeConfig
	//stackFrameFormatter pprof.StackFrameFormatter

	sampleTypesFilter func(string) bool

	previousCache LabelsCache

	startTime time.Time
	endTime   time.Time
	ctx       context.Context

	profile             []byte
	strings             [][]byte
	profileIDLabelIndex int64
	sampleTypesParsed   []valueType
	periodType          valueType
	period              int
	mainBuf             *codec.Buffer
	tmpBuf1             *codec.Buffer
	tmpBuf2             *codec.Buffer

	functions []function
	locations []location

	indexes []int
	types   []int64

	tmpSample sample
	tmpLabel  label

	finder finder

	pp profileParser
}

func NewStreamingParser(config ParserConfig) *MoleculeParser {
	//if config.StackFrameFormatter == nil {//todo
	//	config.StackFrameFormatter = &pprof.UnsafeFunctionNameFormatter{}
	//}
	return &MoleculeParser{
		putter:        config.Putter,
		spyName:       config.SpyName,
		labels:        config.Labels,
		sampleTypes:   config.SampleTypes,
		skipExemplars: config.SkipExemplars,

		previousCache:     make(LabelsCache),
		sampleTypesFilter: filterKnownSamples(config.SampleTypes),
	}
}

func (p *MoleculeParser) GetSampleTypesFilter() func(string) bool {
	return p.sampleTypesFilter
}

func (p *MoleculeParser) SetSampleTypesFilter(f func(string) bool) {
	p.sampleTypesFilter = f
}
func (p *MoleculeParser) ParsePprof(ctx context.Context, startTime, endTime time.Time, bs []byte) error {
	p.startTime = startTime
	p.endTime = endTime
	p.ctx = ctx
	defer func() { p.ctx = nil }()
	if len(bs) < 2 {
		return fmt.Errorf("failed to read pprof profile header")
	}
	if bs[0] == 0x1f && bs[1] == 0x8b {
		gzipr, err := gzip.NewReader(bytes.NewReader(bs))
		if err != nil {
			return fmt.Errorf("failed to create pprof profile zip reader: %w", err)
		}
		defer gzipr.Close()
		buf := bytebufferpool.Get()
		defer bytebufferpool.Put(buf)
		if _, err = io.Copy(buf, gzipr); err != nil {
			return err
		}
		p.profile = buf.Bytes()
		defer func() { p.profile = nil }()
	} else {
		p.profile = bs
	}
	return p.parsePprofDecompressed()
}

func (p *MoleculeParser) parsePprofDecompressed() (err error) {
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("parse panic")
		}
	}()

	p.strings = make([][]byte, 0, 256) // todo sane default? count? reuse?
	p.sampleTypesParsed = make([]valueType, 0, 4)
	p.mainBuf = codec.NewBuffer(nil)
	p.tmpBuf1 = codec.NewBuffer(nil)
	p.tmpBuf2 = codec.NewBuffer(nil)

	if err = p.parseStructs(); err != nil {
		return err
	}
	if err = p.parseFunctionsAndLocations(); err != nil {
		return err
	}
	if err = p.checkKnownSampleTypes(); err != nil {
		return err
	}

	newCache := make(LabelsCache)
	if err = p.parseSamples(newCache); err != nil {
		return err
	}
	return p.iterate(newCache, p.put)
}

// step 1
// - collect strings
// - parse periodType
// - parse sampleType
// - count number of locations and functions

func (p *MoleculeParser) parseStructs() error {
	err := p.pp.parse(p.profile, profileCallbacks{
		string:     p.addString,
		sampleType: p.addSampleType,
		periodType: p.addPeriodType,
	})
	if err == nil {
		p.period = p.pp.period
		p.functions = make([]function, 0, p.pp.nFunctions) //todo reuse these for consecutive parse calls? if cap is enough ?
		p.locations = make([]location, 0, p.pp.nLocations)
	}
	return err
}

func (p *MoleculeParser) addString(s []byte) {
	if bytes.Equal(s, profileIDLabel) {
		p.profileIDLabelIndex = int64(len(p.strings))
	}
	p.strings = append(p.strings, s)
}

func (p *MoleculeParser) addSampleType(st *valueType) {
	p.sampleTypesParsed = append(p.sampleTypesParsed, *st)
}

func (p *MoleculeParser) addPeriodType(pt *valueType) {
	p.periodType = *pt
}

func (p *MoleculeParser) parseFunctionsAndLocations() error {
	err := p.pp.parse(p.profile, profileCallbacks{
		function: p.addFunction,
		location: p.addLocation,
	})
	if err == nil {
		p.finder = NewFinder(p.functions, p.locations)
	}
	return err
}

func (p *MoleculeParser) addFunction(f *function) {
	p.functions = append(p.functions, *f)
}

func (p *MoleculeParser) addLocation(l *location) {
	p.locations = append(p.locations, *l)
}

func (p *MoleculeParser) checkKnownSampleTypes() error {
	p.indexes = make([]int, 0, len(p.sampleTypesParsed))
	p.types = make([]int64, 0, len(p.sampleTypesParsed))
	for i, s := range p.sampleTypesParsed {
		ssType, err := p.string(s.Type)
		if err != nil {
			return err
		}
		if p.sampleTypesFilter(string(ssType)) {
			p.indexes = append(p.indexes, i)
			p.types = append(p.types, s.Type)
		}
	}
	if len(p.indexes) == 0 {
		return fmt.Errorf("unknown sample types")
	}
	p.tmpSample.preAllocate(len(p.indexes))
	//p.tmpValues = make([]int64, len(p.indexes))
	return nil
}

func (p *MoleculeParser) parseSamples(newCache LabelsCache) error {
	p.mainBuf.Reset(p.profile)
	err := molecule.MessageEach(p.mainBuf, func(field int32, value molecule.Value) (bool, error) {
		if profSample == field {
			err := p.parseSampleVT(value.Bytes, newCache)
			if err != nil {
				return false, err
			}
			//p.tmpBuf1.Reset(value.Bytes)
			//
			//err := p.parseSample(p.tmpBuf1, newCache)
			//if err != nil {
			//	return false, err
			//}
		}
		return true, nil
	})
	return err
}

func (p *MoleculeParser) parseSample(buffer *codec.Buffer, newCache LabelsCache) error {
	p.tmpSample.resetSample()
	err := molecule.MessageEach(buffer, func(field int32, value molecule.Value) (bool, error) {
		switch field {
		case sampleLocationID:
			switch value.WireType {
			case codec.WireBytes:
				p.tmpBuf2.Reset(value.Bytes)
				err := molecule.PackedRepeatedEach(p.tmpBuf2, codec.FieldType_UINT64, func(value molecule.Value) (bool, error) {
					err := p.addStackLocation(value.Number)
					if err != nil {
						return false, err
					}
					return true, nil
				})
				if err != nil {
					return false, err
				}
			case codec.WireVarint:
				if err := p.addStackLocation(value.Number); err != nil {
					return false, err
				}
			}

		case sampleValue:
			switch value.WireType {
			case codec.WireBytes:
				p.tmpBuf2.Reset(value.Bytes)
				err := molecule.PackedRepeatedEach(p.tmpBuf2, codec.FieldType_UINT64, func(value molecule.Value) (bool, error) {
					p.tmpSample.tmpValues = append(p.tmpSample.tmpValues, int64(value.Number))
					return true, nil
				})
				if err != nil {
					return false, err
				}
			case codec.WireVarint:
				p.tmpSample.tmpValues = append(p.tmpSample.tmpValues, int64(value.Number))
			}
		case sampleLabel:
			p.tmpBuf2.Reset(value.Bytes)
			l, err := parseLabel(p.tmpBuf2)
			if err != nil {
				return false, err
			}
			if l.v != 0 {
				p.tmpSample.tmpLabels = append(p.tmpSample.tmpLabels, l)
			}
		}
		return true, nil
	})

	reverseStack(p.tmpSample.tmpStack)

	p.createTrees(newCache)
	return err
}

func (p *MoleculeParser) addStackLocation(lID uint64) error {
	loc, ok := p.finder.FindLocation(lID)
	if ok {
		if err := p.addStackFrame(loc.fn1); err != nil {
			return err
		}
		if loc.extraFn != nil {
			for i := 0; i < len(loc.extraFn); i++ {
				fID := loc.extraFn[i]
				if err := p.addStackFrame(fID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (p *MoleculeParser) addStackFrame(fID uint64) error {
	if fID == 0 {
		return nil
	}
	f, ok := p.finder.FindFunction(fID)
	if !ok {
		return nil
	}

	//name, err := p.string(f.name)
	//if err != nil {
	//	return err
	//}
	//p.tmpStack = append(p.tmpStack, name)
	p.tmpSample.tmpStack = append(p.tmpSample.tmpStack, p.strings[f.name])
	return nil
}

func (p *MoleculeParser) string(i int64) ([]byte, error) {
	//if i < 0 || i >= len(p.strings) {
	//	return nil, fmt.Errorf("string out of bound %d", i)
	//}
	return p.strings[i], nil
}

// todo return pointer and resolve strings once
func (p *MoleculeParser) resolveSampleType(v int64) (valueType, bool) {
	for _, vt := range p.sampleTypesParsed {
		if vt.Type == v {
			return vt, true
		}
	}
	return valueType{}, false
}

func (p *MoleculeParser) iterate(newCache LabelsCache, fn func(st valueType, l Labels, t *tree.Tree) (keep bool, err error)) error {
	for stt, entries := range newCache {
		t, ok := p.resolveSampleType(stt)
		if !ok {
			continue
		}

		for h, e := range entries {
			keep, err := fn(t, e.Labels, e.Tree)
			if err != nil {
				return err
			}
			if !keep {
				newCache.Remove(stt, h)
			}
		}
	}
	p.previousCache = newCache
	return nil
}

func (p *MoleculeParser) createTrees(newCache LabelsCache) {
	for i, vi := range p.indexes {
		_ = i
		v := uint64(p.tmpSample.tmpValues[vi])
		if v == 0 {
			continue
		}
		if j := findLabelIndex(p.tmpSample.tmpLabels, p.profileIDLabelIndex); j >= 0 {
			newCache.GetOrCreateTree(p.types[i], CutLabel(p.tmpSample.tmpLabels, j)).InsertStack(p.tmpSample.tmpStack, v)
			if p.skipExemplars {
				continue
			}
		}
		newCache.GetOrCreateTree(p.types[i], p.tmpSample.tmpLabels).InsertStack(p.tmpSample.tmpStack, v)
	}
}

func (p *MoleculeParser) put(st valueType, l Labels, t *tree.Tree) (keep bool, err error) {
	sampleTypeBytes, err := p.string(st.Type)
	if err != nil {
		return false, err
	}
	sampleType := string(sampleTypeBytes) //todo convert once
	sampleTypeConfig, ok := p.sampleTypes[sampleType]
	if !ok {
		return false, fmt.Errorf("sample value type is unknown")
	}
	pi := storage.PutInput{
		StartTime: p.startTime,
		EndTime:   p.endTime,
		SpyName:   p.spyName,
		Val:       t,
	}
	// Cumulative profiles require two consecutive samples,
	// therefore we have to cache this trie.
	if sampleTypeConfig.Cumulative {
		prev, found := p.previousCache.Get(st.Type, l.Hash())
		if !found {
			// Keep the current entry in cache.
			return true, nil
		}
		// Take diff with the previous tree.
		// The result is written to prev, t is not changed.
		pi.Val = prev.Diff(t)
	}
	pi.AggregationType = sampleTypeConfig.Aggregation
	if sampleTypeConfig.Sampled {
		pi.SampleRate = p.sampleRate()
	}
	if sampleTypeConfig.DisplayName != "" {
		sampleType = sampleTypeConfig.DisplayName
	}
	if sampleTypeConfig.Units != "" {
		pi.Units = sampleTypeConfig.Units
	} else {
		// TODO(petethepig): this conversion is questionable
		unitsBytes, err := p.string(st.unit)
		pi.Units = metadata.Units(unitsBytes)
		if err != nil {
			return false, err
		}
	}
	pi.Key = p.buildName(sampleType, p.ResolveLabels(l))
	err = p.putter.Put(p.ctx, &pi)
	return sampleTypeConfig.Cumulative, err
}

func (p *MoleculeParser) ResolveLabels(l Labels) map[string]string {
	m := make(map[string]string, len(l))
	for _, l := range l {
		if l.k != 0 {
			sk, err := p.string(l.k)
			if err != nil {
				continue
			}
			sv, err := p.string(l.v)
			if err != nil {
				continue
			}
			m[string(sk)] = string(sv)
		}
	}
	return m
}

func (p *MoleculeParser) buildName(sampleTypeName string, labels map[string]string) *segment.Key {
	for k, v := range p.labels {
		labels[k] = v
	}
	labels["__name__"] += "." + sampleTypeName
	return segment.NewKey(labels)
}

func (p *MoleculeParser) sampleRate() uint32 {
	if p.period <= 0 || p.periodType.unit <= 0 {
		return 0
	}
	sampleUnit := time.Nanosecond
	u, err := p.string(p.periodType.unit)
	if err == nil {
		switch string(u) { // todo convert once?
		case "microseconds":
			sampleUnit = time.Microsecond
		case "milliseconds":
			sampleUnit = time.Millisecond
		case "seconds":
			sampleUnit = time.Second
		}
	}

	return uint32(time.Second / (sampleUnit * time.Duration(p.period)))
}

func filterKnownSamples(sampleTypes map[string]*tree.SampleTypeConfig) func(string) bool {
	return func(s string) bool {
		_, ok := sampleTypes[s]
		return ok
	}
}

func findLabelIndex(tmpLabels []label, k int64) int {
	for i, l := range tmpLabels {
		if l.k == k {
			return i
		}
	}
	return -1
}
func reverseStack(s [][]byte) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
