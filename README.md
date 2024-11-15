# go-raw-osfile-connect
This project demonstrates sporadic waits using raw `os.File` upon connect a non-blocking TCP socket 

Tested on Linux arm64 and Linux amd64 with go versions 1.22.6 and 1.23.3

## To build and start the example server:

    - cd tcp_server
    - go build tcp_server.go
    - ./tcp_server

## To build and start the example raw client:

    - cd tcp_client_raw
    - go build tcp_client_raw.go
    - ./tcp_client_raw

