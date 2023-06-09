FROM golang:latest

COPY cmd/client/pushprox-client /usr/local/bin/
COPY cmd/proxy/pushprox-proxy /usr/local/bin/

CMD [ "pushprox-client" ]