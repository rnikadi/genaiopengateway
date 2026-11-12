FROM gcr.io/oss-fuzz-base/base-builder-go
# go.mod asks for a newer Go than the image may have; the go command
# downloads it.
ENV GOTOOLCHAIN=auto
COPY . $SRC/genaiopengateway
WORKDIR $SRC/genaiopengateway
COPY .clusterfuzzlite/build.sh $SRC/
