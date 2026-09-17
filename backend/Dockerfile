FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /bin/gt-server .

FROM alpine:3.20
RUN adduser -D -u 10001 app && mkdir -p /data && chown app:app /data
USER app
COPY --from=build /bin/gt-server /usr/local/bin/gt-server
EXPOSE 7105
CMD ["gt-server"]
