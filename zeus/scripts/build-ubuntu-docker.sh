#!/bin/bash

if [ -z "$NODPI" ]; then
  NODPI=true
fi

if $NODPI; then
  echo "[INFO] copying the docker/ubuntu-nodpi/Dockerfile into the project root"
  cp docker/ubuntu-nodpi/Dockerfile Dockerfile
else
  echo "[INFO] copying the docker/ubuntu/Dockerfile into the project root"
  cp docker/ubuntu/Dockerfile Dockerfile
fi

# generate version, add update the VERSION env var in the Dockerfile that was moved to the project root
zeus gen-version

tag="dreadl0ck/netcap:ubuntu-${VERSION}"

echo "[INFO] $tag args: ${ARGS}"

# in case of cache annoyances:
# docker rm -f $(docker ps -a -q)
# docker rmi -f $(docker images -a -q)

# build image
# dont quote ARGS or passing arguments wont work anymore
docker build ${ARGS} -t "$tag" .
if (( $? != 0 )); then
	echo "[ERROR] building container failed"
	exit 1
fi

echo "[INFO] running docker image $tag"

docker run "$tag"

# echo "[INFO] docker images"
# docker image ls

# grab container ID
echo "[INFO] looking for $tag container ID"
CONTAINER_ID=$(docker ps -a -f ancestor=$tag -q --latest)
if [[ $CONTAINER_ID == "" ]]; then
	echo "[ERROR] no docker container found"
	exit 1
fi

ARCHIVE="netcap_${VERSION}_linux_amd64_libc"

echo "[INFO] preparing dist folder, CONTAINER_ID: $CONTAINER_ID, archive: $ARCHIVE"

# clean up
rm -rf dist/${ARCHIVE}

# create path in dist
mkdir -p dist/${ARCHIVE}

# copy binaries from container
docker cp $CONTAINER_ID:/usr/bin/net dist/${ARCHIVE}/net

# remove container
docker rm $CONTAINER_ID

cp LICENSE dist/${ARCHIVE}
cp README.md dist/${ARCHIVE}

cd dist

# create tar archive for linux
tar -cvf ${ARCHIVE}.tar.gz ${ARCHIVE}

# add checksum - goreleaser needs to be patched for this to work
# by default the checksums.txt file is truncated when being opened
shasum -a 256 ${ARCHIVE}.tar.gz >> checksums.txt

# remove license and readme from binary folder
rm ${ARCHIVE}/LICENSE
rm ${ARCHIVE}/README.md

echo "[INFO] pushing container to docker registry"
docker push "$tag"

#echo "[INFO] removing docker image"
#docker image rm "$tag"

echo "[INFO] done"
