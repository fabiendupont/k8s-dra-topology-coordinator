FROM golang:1.25 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build -ldflags="-s -w -extldflags=-static" -trimpath -o /bin/nodepartition-controller ./cmd/nodepartition-controller

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/nodepartition-controller /usr/local/bin/nodepartition-controller
ENTRYPOINT ["nodepartition-controller"]
