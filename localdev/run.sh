#!/bin/bash

INFRA_SVCS="redis postgres pulsar"
ARMADA_SVCS="server lookout lookout-ingester executor binoculars jobservice event-ingester"
COMPOSE_FILE=""

# make the dir containing this file the CWD
cd "$(dirname "${0}")" || exit
# get the first command-line argument, or 'default' if not available
command=${1:-default}

# Switch compose files if requested by command-line arg
case "$command" in
  "debug")
    echo "starting debug compose environment"
    COMPOSE_FILE="-f docker-compose.yaml -f docker-compose.debug.yaml"
    # make golang image with delve
    docker build -t golang:1.18-delve .
    ;;
  *)
    echo "starting compose environment"
    # default action
    ;;
esac

# start the kubernetes cluster if needed
kind get clusters | grep armada-test &> /dev/null
if [ $? -ne 0 ];
then
    scripts/kind-start.sh
fi

# select arm64 image for pulsar if needed
uname -a | grep "arm64" &> /dev/null
if [ $? -eq 0 ];
then
    export PULSAR_IMAGE="kezhenxu94/pulsar"
fi

# see if pulsar is already up, in which case we don't need to sleep
SLEEP_TIME=0
docker-compose ps | grep -E "pulsar.+running" &> /dev/null
if [ $? -ne 0 ];
then
    echo "Pausing for pulsar start up ..."
    SLEEP_TIME=50
fi

docker-compose $COMPOSE_FILE up -d $INFRA_SVCS
sleep $SLEEP_TIME
docker-compose $COMPOSE_FILE up -d $ARMADA_SVCS
