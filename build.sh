#!/bin/bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o distribution .
echo "编译完成: ./distribution (linux/amd64)"
