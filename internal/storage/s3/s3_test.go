package s3_test

import (
	"context"
	"os"
	"testing"

	s3store "github.com/Mininglamp-OSS/octo-docs-html/internal/storage/s3"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/storagetest"
)

// TestS3Contract runs the blob contract against a real S3-compatible store when
// OCTO_TEST_S3_BUCKET is set; otherwise it is skipped.
func TestS3Contract(t *testing.T) {
	bucket := os.Getenv("OCTO_TEST_S3_BUCKET")
	if bucket == "" {
		t.Skip("set OCTO_TEST_S3_BUCKET (and S3 env) to run the S3 contract test")
	}
	ctx := context.Background()
	store, err := s3store.Open(ctx, s3store.Options{
		Bucket:         bucket,
		Region:         envOr("OCTO_TEST_S3_REGION", "us-east-1"),
		Endpoint:       os.Getenv("OCTO_TEST_S3_ENDPOINT"),
		ForcePathStyle: true,
		AccessKeyID:    os.Getenv("OCTO_TEST_S3_ACCESS_KEY_ID"),
		SecretKey:      os.Getenv("OCTO_TEST_S3_SECRET_ACCESS_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	storagetest.RunBlob(t, store)
}

func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}
