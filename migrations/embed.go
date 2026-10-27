// Package migrations 把 DDL 编进二进制，避免部署时漏带 SQL 文件。
// **DDL 的唯一来源是这个目录** —— 迁移服务独占执行权。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
