# ---------------------------------------------------------------#
# This simulation runs the Ethereum python execution-spec-tests. #
# ---------------------------------------------------------------#

# 1) Create pyspec builder container.
FROM golang:1-alpine as builder
RUN apk add --update git ca-certificates gcc musl-dev linux-headers

# Build the pyspec simulator executable.
ADD . /source
WORKDIR /source/pyspec
RUN go build -v .

# 2) Create the simulator container.
FROM alpine:latest as simulator
RUN apk add --update wget
ADD ./pyspec /pyspec
WORKDIR /pyspec
COPY --from=builder /source/pyspec/pyspec .

# To run locally generated fixtures, comment the following RUN lines and
# uncomment the ADD line.
# Download the latest fixture release.
RUN wget https://github.com/spencer-tb/execution-spec-tests/releases/latest/download/fixtures-cancun.tar.gz
RUN tar -xzvf fixtures-cancun.tar.gz
RUN mv fixtures /fixtures

# ADD ./pyspec/fixtures /fixtures

# Point to executable and test fixtures.
ENV TESTPATH /fixtures
ENTRYPOINT ["./pyspec"]