# build stage
FROM golang:1.20.2-alpine3.17 as builder

ENV GO111MODULE=on

WORKDIR /app

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build

# final stage
FROM scratch

COPY --from=builder /app/imagepullsecret-patcher /app/

ENTRYPOINT ["/app/imagepullsecret-patcher"]
