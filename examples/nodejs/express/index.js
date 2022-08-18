/* eslint-disable */
const Pyroscope = require('@pyroscope/nodejs');

const port = process.env['PORT'] || 3000;

const region = process.env['REGION'] || 'default';

const express = require('express');
const morgan = require('morgan');

const app = express();
app.use(morgan('dev'));
app.get('/', (req, res) => {
  res.send('Available routes are: /bike, /car, /scooter');
});

function genericSearchHandler(p) {
  return function (req, res) {
    const time = +new Date() + p * 1000;
    let i = 0;
    while (+new Date() < time) {
      i = i + Math.random();
    }
    res.send('Vehicle found');
  };
}

Pyroscope.init({
  appName: 'nodejs',
  serverAddress: process.env['PYROSCOPE_SERVER'] || 'http://pyroscope:4040',
  tags: { region },
});

Pyroscope.startCpuProfiling();
Pyroscope.startHeapProfiling();

app.get('/bike', function bikeSearchHandler(req, res) {
  Pyroscope.tagWrapper({ vehicle: 'bike' }, () =>
    genericSearchHandler(0.5)(req, res)
  );
});
app.get('/car', function carSearchHandler(req, res) {
  Pyroscope.tagWrapper({ vehicle: 'car' }, () =>
    genericSearchHandler(0.2)(req, res)
  );
});
app.get('/scooter', function scooterSearchHandler(req, res) {
  Pyroscope.tagWrapper({ vehicle: 'scooter' }, () =>
    genericSearchHandler(0.1)(req, res)
  );
});

app.listen(port, () => {
  console.log(
    `Server has started on port ${port}, use http://localhost:${port}`
  );
});
