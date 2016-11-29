FROM golang:1.7

MAINTAINER Shaun Crampton <shaun@tigera.io>

ARG UID
ARG GID

# Install build pre-reqs:
# - bsdmainutils contains the "column" command, used to format the coverage
#   data.
RUN apt-get update && \
    apt-get install -y bsdmainutils && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

RUN go get github.com/Masterminds/glide \
           github.com/onsi/ginkgo/ginkgo \
           github.com/onsi/gomega \
           github.com/wadey/gocovmerge

# glide requires the current user to exist inside the container
# use `--force` and `-o` since tests can run under root and command will fail with duplicate error
RUN groupadd --force --gid=$GID user && useradd -o --home=/ --gid=$GID --uid=$UID user

# Make sure the normal user has write access to the GOPATH.  Needs to be done
# at the end because the above commands will write into this directory as root.
RUN chmod -R a+wX $GOPATH /usr/local/go

# Disable cgo so that binaries we build will be fully static.
ENV CGO_ENABLED=0

WORKDIR /go/src/github.com/projectcalico/felix
