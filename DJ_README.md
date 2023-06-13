# Info
- The code adaption is done on the branch `adaption_from_tag` which is branched from the tag `https://github.com/rancher/PushProx/tree/v0.1.0-rancher2`
- Reason is to keep it in sync with the rancher helm charts `https://github.com/rancher/charts/tree/dev-v2.7/charts/rancher-pushprox`

# New parts
- Dockerfile to host client and proxy bins
- Code adaption in client/main.go to call cluster prometheus federate endpoint

# Prerequisites local dev machine
- docker installed and running
- docker login to `deepjudge.azurecr.io` repository done

# Create an build new version on local machine (Linux, MacOS, Windows)
- adapt code for client or proxy on branch `adaption_from_tag`
- In the repository change env variable VERSION to new version
- run `./build_dj_version.sh
- this will build, create docker image and push it to azure container registry

# Put new version in rancher-pushprox helm chart (deepjudge_cd)
- adapt version in values.yaml under clients/image/tag and proxy/image/tag
