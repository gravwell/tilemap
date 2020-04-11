#!/bin/bash

CGO_ENABLED=0 go build
docker build --squash -t gravwell/tileserver .
