// Package s3 implements storage.BlobStore on any S3-compatible object store
// (AWS S3, MinIO, R2). Object keys hash the slug so an unexpected slug can never
// produce a key outside the docs/ prefix. S3 PUT is atomic — no half-writes.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// Options configure the S3 blob store.
type Options struct {
	Bucket         string
	Region         string
	Endpoint       string
	ForcePathStyle bool
	AccessKeyID    string
	SecretKey      string
}

// Store is an S3-compatible BlobStore.
type Store struct {
	client *awss3.Client
	bucket string
}

var _ storage.BlobStore = (*Store)(nil)

var versionKeyRe = regexp.MustCompile(`/v(\d+)/index\.html$`)

// Open builds an S3 client from opts.
func Open(ctx context.Context, opts Options) (*Store, error) {
	var loadOpts []func(*awscfg.LoadOptions) error
	loadOpts = append(loadOpts, awscfg.WithRegion(opts.Region))
	if opts.AccessKeyID != "" {
		loadOpts = append(loadOpts, awscfg.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(opts.AccessKeyID, opts.SecretKey, ""),
		))
	}
	cfg, err := awscfg.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
		o.UsePathStyle = opts.ForcePathStyle
	})
	return &Store{client: client, bucket: opts.Bucket}, nil
}

// Health verifies the bucket is reachable (used by the readiness probe). Open
// intentionally does not validate connectivity, so this is the first real check.
func (s *Store) Health(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &awss3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	}); err != nil {
		return fmt.Errorf("s3 head bucket %q: %w", s.bucket, err)
	}
	return nil
}

func (s *Store) prefixFor(slug string) string {
	return "docs/" + storage.HashSlug(slug)
}

func (s *Store) keyFor(slug string, version int) string {
	return s.prefixFor(slug) + "/v" + strconv.Itoa(version) + "/index.html"
}

// draftKeyFor is the mutable draft slot. The "draft" segment is deliberately not
// "v<digits>", so ListVersions's version regex never matches it.
func (s *Store) draftKeyFor(slug string) string {
	return s.prefixFor(slug) + "/draft/index.html"
}

// assetKeyFor is a content-addressed asset key. It lives under the same per-slug
// prefix as versions, so DeleteDoc's prefix sweep removes assets too, and the
// "assets/" segment never matches versionKeyRe.
func (s *Store) assetKeyFor(slug, sha256 string) string {
	return s.prefixFor(slug) + "/assets/" + sha256
}

func isNotFound(err error) bool {
	if _, ok := errors.AsType[*types.NoSuchKey](err); ok {
		return true
	}
	if _, ok := errors.AsType[*types.NotFound](err); ok {
		return true
	}
	if ae, ok := errors.AsType[smithy.APIError](err); ok {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

// PutDoc writes a document version.
func (s *Store) PutDoc(ctx context.Context, slug string, version int, html string) (int64, error) {
	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.keyFor(slug, version)),
		Body:        strings.NewReader(html),
		ContentType: aws.String("text/html; charset=utf-8"),
	})
	if err != nil {
		return 0, err
	}
	return int64(len(html)), nil
}

// GetDoc fetches a document version, returning (html, found, error).
func (s *Store) GetDoc(ctx context.Context, slug string, version int) (string, bool, error) {
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.keyFor(slug, version)),
	})
	if err != nil {
		if isNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

// HeadDoc reports a version's size and existence.
func (s *Store) HeadDoc(ctx context.Context, slug string, version int) (int64, bool, error) {
	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.keyFor(slug, version)),
	})
	if err != nil {
		if isNotFound(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if out.ContentLength != nil {
		return *out.ContentLength, true, nil
	}
	return 0, true, nil
}

// PutDraft writes (overwrites) the mutable draft slot for a slug.
func (s *Store) PutDraft(ctx context.Context, slug string, html string) (int64, error) {
	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.draftKeyFor(slug)),
		Body:        strings.NewReader(html),
		ContentType: aws.String("text/html; charset=utf-8"),
	})
	if err != nil {
		return 0, err
	}
	return int64(len(html)), nil
}

// GetDraft fetches the draft slot, returning (html, found, error).
func (s *Store) GetDraft(ctx context.Context, slug string) (string, bool, error) {
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.draftKeyFor(slug)),
	})
	if err != nil {
		if isNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

// DeleteDraft removes the draft slot. A missing draft is not an error.
func (s *Store) DeleteDraft(ctx context.Context, slug string) error {
	_, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.draftKeyFor(slug)),
	})
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// PutAsset stores raw asset bytes. Content-addressed, so an overwrite of the same
// key is a harmless no-op write of identical bytes.
func (s *Store) PutAsset(ctx context.Context, slug, sha256 string, data []byte) error {
	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.assetKeyFor(slug, sha256)),
		Body:   bytes.NewReader(data),
	})
	return err
}

// GetAsset fetches raw asset bytes, returning (bytes, found, error).
func (s *Store) GetAsset(ctx context.Context, slug, sha256 string) ([]byte, bool, error) {
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.assetKeyFor(slug, sha256)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// DeleteAsset removes one asset. A missing asset is not an error.
func (s *Store) DeleteAsset(ctx context.Context, slug, sha256 string) error {
	_, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.assetKeyFor(slug, sha256)),
	})
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// ListVersions returns the versions present for a slug, ascending.
func (s *Store) ListVersions(ctx context.Context, slug string) ([]int, error) {
	prefix := s.prefixFor(slug) + "/"
	var out []int
	var token *string
	for {
		res, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, obj := range res.Contents {
			if obj.Key == nil {
				continue
			}
			if m := versionKeyRe.FindStringSubmatch(*obj.Key); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil {
					out = append(out, n)
				}
			}
		}
		if res.IsTruncated != nil && *res.IsTruncated {
			token = res.NextContinuationToken
		} else {
			break
		}
	}
	sort.Ints(out)
	return out, nil
}

// DeleteDoc removes all versions for a slug.
func (s *Store) DeleteDoc(ctx context.Context, slug string) error {
	prefix := s.prefixFor(slug) + "/"
	var token *string
	for {
		res, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return err
		}
		if len(res.Contents) > 0 {
			ids := make([]types.ObjectIdentifier, 0, len(res.Contents))
			for _, obj := range res.Contents {
				ids = append(ids, types.ObjectIdentifier{Key: obj.Key})
			}
			if _, err := s.client.DeleteObjects(ctx, &awss3.DeleteObjectsInput{
				Bucket: aws.String(s.bucket),
				Delete: &types.Delete{Objects: ids},
			}); err != nil {
				return err
			}
		}
		if res.IsTruncated != nil && *res.IsTruncated {
			token = res.NextContinuationToken
		} else {
			break
		}
	}
	return nil
}
