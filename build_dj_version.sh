export GOOS=linux 
export GOARCH=amd64
export VERSION=0.8

# build client
cd cmd/client
go build
mv client pushprox-client
cd ../..

# build proxy
cd cmd/proxy
go build
mv proxy pushprox-proxy
cd ../..

# build docker image
docker buildx build -t pushprox_dj:$VERSION . --platform=linux/amd64
docker tag pushprox_dj:$VERSION deepjudge.azurecr.io/pushprox_dj:$VERSION
docker push deepjudge.azurecr.io/pushprox_dj:$VERSION
