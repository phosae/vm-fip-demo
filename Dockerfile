FROM alpine:3.16
WORKDIR /
COPY ./bin/manager .

ENTRYPOINT ["/manager"]
