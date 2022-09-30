FROM alpine:3.16

RUN apk --no-cache add ca-certificates

USER nobody

COPY  --chown=nobody:nobody _output/flagger .

ENTRYPOINT ["./flagger"]
