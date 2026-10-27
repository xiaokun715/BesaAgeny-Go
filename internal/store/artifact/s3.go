// Package artifact 把工具大结果与节点产物存到 S3/MinIO。
//
// 分布式下**不能返回本地路径**：模型拿到路径却换了个节点，读不到文件。
package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3 struct {
	client *minio.Client
	bucket string
	prefix string
}

func OpenS3(ctx context.Context, endpoint, accessKey, secretKey, bucket, prefix string, useSSL bool) (*S3, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("S3 endpoint 不能为空")
	}
	if bucket == "" {
		bucket = "besa-agent"
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, err
	}
	existsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	exists, err := client.BucketExists(existsCtx, bucket)
	if err != nil {
		return nil, fmt.Errorf("检查 bucket: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(existsCtx, bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("创建 bucket: %w", err)
		}
	}
	if prefix == "" {
		prefix = "spill"
	}
	return &S3{client: client, bucket: bucket, prefix: prefix}, nil
}

// Put 内容寻址：同一份内容只存一份。
func (s *S3) Put(ctx context.Context, name string, data []byte, contentType string) (string, error) {
	sum := sha256.Sum256(data)
	key := fmt.Sprintf("%s/%s/%s", s.prefix, hex.EncodeToString(sum[:8]), name)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return "", err
	}
	return "s3://" + s.bucket + "/" + key, nil
}

func (s *S3) Get(ctx context.Context, uri string) ([]byte, error) {
	bucket, key, err := parseURI(uri)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(obj)
}

// Presign 生成短期签名 URL（下载凭证不落日志）。
func (s *S3) Presign(ctx context.Context, uri string, ttl time.Duration) (string, error) {
	bucket, key, err := parseURI(uri)
	if err != nil {
		return "", err
	}
	u, err := s.client.PresignedGetObject(ctx, bucket, key, ttl, url.Values{})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func parseURI(uri string) (string, string, error) {
	if !strings.HasPrefix(uri, "s3://") {
		return "", "", fmt.Errorf("不是 s3:// URI: %s", uri)
	}
	rest := strings.TrimPrefix(uri, "s3://")
	i := strings.IndexByte(rest, '/')
	if i <= 0 {
		return "", "", fmt.Errorf("URI 缺少对象键: %s", uri)
	}
	return rest[:i], rest[i+1:], nil
}
