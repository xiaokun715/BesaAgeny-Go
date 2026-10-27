SHELL := /bin/sh
BIN := bin/besa

.PHONY: help build test vet fmt fmt-check proto run doctor up down migrate clean

help:
	@echo "make build     编译 bin/besa"
	@echo "make test      运行全部测试"
	@echo "make vet       go vet"
	@echo "make proto     重新生成 gRPC 代码（需要 protoc + 插件）"
	@echo "make run       本地跑一条任务（mock）"
	@echo "make doctor    显示模型/工具/策略装配结果"
	@echo "make up        docker compose 起全套分布式服务"
	@echo "make down      停止服务"
	@echo "make migrate   执行数据库迁移"

build:
	go build -trimpath -o $(BIN) ./cmd/besa

test:
	go test ./...

vet:
	go vet ./...

fmt:
	find . -name '*.go' -not -path './api/proto/*' -exec gofmt -w {} +

proto:
	export PATH="$$(go env GOPATH)/bin:$$PATH" && \
	protoc -I api/proto/v1 \
	  --go_out=. --go_opt=module=github.com/besa-ai/besa-agent \
	  --go-grpc_out=. --go-grpc_opt=module=github.com/besa-ai/besa-agent \
	  api/proto/v1/besa.proto

run:
	BESA_ENV=test go run ./cmd/besa run "帮我为登录接口设计测试用例"

doctor:
	BESA_ENV=test go run ./cmd/besa doctor

up:
	docker compose up --build -d

down:
	docker compose down

migrate:
	go run ./cmd/besa --env prod migrate

clean:
	rm -rf bin
