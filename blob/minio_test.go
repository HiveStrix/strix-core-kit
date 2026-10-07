package blob

import "github.com/minio/minio-go/v7"

func minioMakeBucketOptions() minio.MakeBucketOptions {
	return minio.MakeBucketOptions{Region: "us-east-1"}
}
func minioStatOptions() minio.StatObjectOptions { return minio.StatObjectOptions{} }
