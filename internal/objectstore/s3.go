package objectstore

import (
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	UseSSL    bool
}

type S3 struct {
	client *minio.Client
	bucket string
}

func NewS3(config S3Config) (*S3, error) {
	client, err := minio.New(config.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""),
		Secure: config.UseSSL,
		Region: config.Region,
	})
	if err != nil {
		return nil, err
	}
	return &S3{client: client, bucket: config.Bucket}, nil
}

func (s *S3) Put(ctx context.Context, key string, contents io.Reader, size int64, contentType string) error {
	if !validKey(key) {
		return fmt.Errorf("unsafe object key %q", key)
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, contents, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, string, error) {
	if !validKey(key) {
		return nil, "", fmt.Errorf("unsafe object key %q", key)
	}
	details, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, "", err
	}
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", err
	}
	return object, details.ContentType, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if !validKey(key) {
		return fmt.Errorf("unsafe object key %q", key)
	}
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}
