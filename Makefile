#
# Copyright 2016-present the original author or authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

# set default shell
SHELL = bash -e -o pipefail

# Variables
VERSION                    ?= $(shell cat ./VERSION)

DOCKER_LABEL_VCS_DIRTY     = false
ifneq ($(shell git ls-files --others --modified --exclude-standard 2>/dev/null | wc -l | sed -e 's/ //g'),0)
    DOCKER_LABEL_VCS_DIRTY = true
endif
## Docker related
DOCKER_EXTRA_ARGS          ?=
DOCKER_REGISTRY            ?=
DOCKER_REPOSITORY          ?=
DOCKER_TAG                 ?= ${VERSION}$(shell [[ ${DOCKER_LABEL_VCS_DIRTY} == "true" ]] && echo "-dirty" || true)
SIMULATEDONU_IMAGENAME     := ${DOCKER_REGISTRY}${DOCKER_REPOSITORY}voltha-adapter-simulated-onu

## Docker labels. Only set ref and commit date if committed
DOCKER_LABEL_VCS_URL       ?= $(shell git remote get-url $(shell git remote))
DOCKER_LABEL_VCS_REF       = $(shell git rev-parse HEAD)
DOCKER_LABEL_BUILD_DATE    ?= $(shell date -u "+%Y-%m-%dT%H:%M:%SZ")
DOCKER_LABEL_COMMIT_DATE   = $(shell git show -s --format=%cd --date=iso-strict HEAD)

# Default is GO111MODULE=auto, which will refuse to use go mod if running
# go less than 1.13.0 and this repository is checked out in GOPATH. For now,
# force module usage. This affects commands executed from this Makefile, but
# not the environment inside the Docker build (which does not build from
# inside a GOPATH).
export GO111MODULE=on

DOCKER_BUILD_ARGS ?= \
	${DOCKER_EXTRA_ARGS} \
	--build-arg org_label_schema_version="${VERSION}" \
	--build-arg org_label_schema_vcs_url="${DOCKER_LABEL_VCS_URL}" \
	--build-arg org_label_schema_vcs_ref="${DOCKER_LABEL_VCS_REF}" \
	--build-arg org_label_schema_build_date="${DOCKER_LABEL_BUILD_DATE}" \
	--build-arg org_opencord_vcs_commit_date="${DOCKER_LABEL_COMMIT_DATE}" \
	--build-arg org_opencord_vcs_dirty="${DOCKER_LABEL_VCS_DIRTY}"

DOCKER_BUILD_ARGS_LOCAL ?= ${DOCKER_BUILD_ARGS} \
	--build-arg LOCAL_PYVOLTHA=${LOCAL_PYVOLTHA} \
	--build-arg LOCAL_PROTOS=${LOCAL_PROTOS}

.PHONY: simulated_onu local-protos

# This should to be the first and default target in this Makefile
help:
	@echo "Usage: make [<target>]"
	@echo "where available targets are:"
	@echo
	@echo "build                : Build the docker images."
	@echo "                         - If this is the first time you are building, choose 'make build' option."
	@echo "simulated_onu        : Build the simulated_onu docker image"
	@echo "clean                : Remove files created by the build and tests"
	@echo "docker-push          : Push the docker images to an external repository"
	@echo "lint-dockerfile      : Perform static analysis on Dockerfiles"
	@echo "lint-style           : Verify code is properly gofmt-ed"
	@echo "lint-sanity          : Verify that 'go vet' doesn't report any issues"
	@echo "lint-mod             : Verify the integrity of the 'mod' files"
	@echo "lint                 : Shorthand for lint-style & lint-sanity"
	@echo "test                 : Generate reports for all go tests"
	@echo

## Local Development Helpers
local-protos:
ifdef LOCAL_PROTOS
	mkdir -p vendor/github.com/opencord/voltha-protos/go
	cp -r ${GOPATH}/src/github.com/opencord/voltha-protos/go/* vendor/github.com/opencord/voltha-protos/go
endif

## Docker targets

build: docker-build

docker-build: simulated_onu

simulated_onu: local-protos
	docker build $(DOCKER_BUILD_ARGS) -t ${SIMULATEDONU_IMAGENAME}:${DOCKER_TAG} -t ${SIMULATEDONU_IMAGENAME}:latest -f docker/Dockerfile.simulated_onu .

docker-push:
	docker push ${SIMULATEDONU_IMAGENAME}:${DOCKER_TAG}

## lint and unit tests

PATH:=$(GOPATH)/bin:$(PATH)
HADOLINT=$(shell PATH=$(GOPATH):$(PATH) which hadolint)
lint-dockerfile:
ifeq (,$(shell PATH=$(GOPATH):$(PATH) which hadolint))
	mkdir -p $(GOPATH)/bin
	curl -o $(GOPATH)/bin/hadolint -sNSL https://github.com/hadolint/hadolint/releases/download/v1.17.1/hadolint-$(shell uname -s)-$(shell uname -m)
	chmod 755 $(GOPATH)/bin/hadolint
endif
	@echo "Running Dockerfile lint check ..."
	@hadolint $$(find . -name "Dockerfile.*")
	@echo "Dockerfile lint check OK"

lint-style:
ifeq (,$(shell which gofmt))
	go get -u github.com/golang/go/src/cmd/gofmt
endif
	@echo "Running style check..."
	@gofmt_out="$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))" ;\
	if [ ! -z "$$gofmt_out" ]; then \
	  echo "$$gofmt_out" ;\
	  echo "Style check failed on one or more files ^, run 'go fmt' to fix." ;\
	  exit 1 ;\
	fi
	@echo "Style check OK"

lint-sanity:
	@echo "Running sanity check..."
	@go vet -mod=vendor ./...
	@echo "Sanity check OK"

lint-mod:
	@echo "Running dependency check..."
	@go mod verify
	@echo "Dependency check OK"

lint: lint-style lint-sanity lint-mod lint-dockerfile

GO_JUNIT_REPORT:=$(shell which go-junit-report)
GOCOVER_COBERTURA:=$(shell which gocover-cobertura)

test:
ifeq (,$(GO_JUNIT_REPORT))
	go get -u github.com/jstemmer/go-junit-report
	@GO_JUNIT_REPORT=$(GOPATH)/bin/go-junit-report
endif
ifeq (,$(GOCOVER_COBERTURA))
	go get -u github.com/t-yuki/gocover-cobertura
	@GOCOVER_COBERTURA=$(GOPATH)/bin/gocover-cobertura
endif
	@mkdir -p ./tests/results
	@go test -mod=vendor -v -coverprofile ./tests/results/go-test-coverage.out -covermode count ./... 2>&1 | tee ./tests/results/go-test-results.out ;\
	RETURN=$$? ;\
	$(GO_JUNIT_REPORT) < ./tests/results/go-test-results.out > ./tests/results/go-test-results.xml ;\
	$(GOCOVER_COBERTURA) < ./tests/results/go-test-coverage.out > ./tests/results/go-test-coverage.xml ;\
	exit $$RETURN

clean:

distclean: clean

# end file
