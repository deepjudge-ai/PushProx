FROM ubuntu:latest

COPY bin/pushprox-client /usr/local/bin/
COPY bin/pushprox-proxy /usr/local/bin/

CMD [ "pushprox-client" ]