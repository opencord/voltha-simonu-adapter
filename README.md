# DEPRECATED

[![No Maintenance Intended](http://unmaintained.tech/badge.svg)](http://unmaintained.tech/)

`VOLTHA 2.3` was the last official set of releases that  used the `voltha-simonu-adapter` repository.
From `2.4` onwards the [volta-openonu-adapter-go](https://github.com/opencord/voltha-openonu-adapter-go) as the exact same functionality in simulated or physical mode, thus the repo has been deprecated. This codebase is going to be removed after the VOLTHA 2.8 release LTS support ends in December 2022.

# How to Build the Golang based Simulated ONU Adapter

## Working with Go Dependencies
This project uses Go Modules https://github.com/golang/go/wiki/Modules to manage
dependencies. As a local best pratice this project also vendors the dependencies.
If you need to update dependencies please follow the Go Modules best practices
and also perform the following steps before committing a patch:
```bash
go mod tidy
go mod verify
go mod vendor
```

## Building with a Local Copy of `voltha-protos` or `voltha-lib-go`
If you want to build/test using a local copy or `voltha-protos` or `voltha-lib-go`
this can be accomplished by using the environment variables `LOCAL_PROTOS` and
`LOCAL_LIB_GO`. These environment variables should be set to the filesystem
path where the local source is located, e.g.

```bash
LOCAL\_PROTOS=$HOME/src/voltha-protos
LOCAL\_LIB\_GO=$HOME/src/voltha-lib-go
```

When these environment variables are set the vendored versions of these packages
will be removed from the `vendor` directory and replaced by coping the files from
the specified locations to the `vendor` directory. *NOTE:* _this means that
the files in the `vendor` directory are no longer what is in the `git` repository
and it will take manual `git` intervention to put the original files back._
