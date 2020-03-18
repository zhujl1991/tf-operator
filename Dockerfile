# Build the manager binary
FROM golang:1.11.4 as builder

# Copy in the go src
WORKDIR /go/src/github.com/kubeflow/tf-operator
COPY pkg/    pkg/
COPY cmd/    cmd/
COPY vendor/ vendor/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o tf-operator cmd/tf-operator.v1/main.go

# Copy the controller-manager into a thin image
FROM ubuntu:latest
WORKDIR /root/
COPY --from=builder /go/src/github.com/kubeflow/tf-operator .
ENTRYPOINT ["./tf-operator"]
