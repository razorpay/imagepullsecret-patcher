# build stage
FROM c.rzp.io/razorpay/rzp-docker-image-inventory-multi-arch:rzp-golden-image-base-golang-1.25-alpine3.23 AS builder
ENV GO111MODULE=on

WORKDIR /app

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build

# final stage
FROM c.rzp.io/razorpay/rzp-docker-image-inventory-multi-arch:rzp-golden-image-base-golang-1.25-alpine3.23

COPY --from=builder /app/imagepullsecret-patcher /app/

ENTRYPOINT ["/app/imagepullsecret-patcher"]
