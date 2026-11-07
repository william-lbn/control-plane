package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const storageObjectLimit = storageObjectFormatLimit
const storagePartLimit = 8 << 20

// This credential must be provisioned with ListBucket/GetObject/PutObject only
// on the product bucket. It must never be a Pageserver or MinIO root credential.
type storageBlobConfig struct {
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	LabHTTP   bool   `json:"lab_http"`
}
type storageBlobs struct {
	client    *minio.Client
	bucket    string
	gcJournal *storageGCJournal
}

func loadStorageBlobs() (*storageBlobs, error) {
	filename := os.Getenv("NEON_OBJECT_STORAGE_CONFIG_FILE")
	if filename == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		return nil, errors.New("product blob credential file unavailable")
	}
	var config storageBlobConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil {
		return nil, errors.New("invalid product blob configuration")
	}
	if err = prepareStoragePayloadDirectory(); err != nil {
		return nil, err
	}
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && config.LabHTTP)) || !regexp.MustCompile(`^neon-product-[a-z0-9-]{1,40}$`).MatchString(config.Bucket) || config.Region == "" || len(config.AccessKey) < 8 || len(config.SecretKey) < 16 {
		return nil, errors.New("product blob endpoint, dedicated bucket and restricted credentials are required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.MaxConnsPerHost = 16
	client, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""), Secure: u.Scheme == "https", Region: config.Region, Transport: transport, BucketLookup: minio.BucketLookupPath})
	if err != nil {
		return nil, errors.New("could not configure product blob client")
	}
	return &storageBlobs{client: client, bucket: config.Bucket}, nil
}
func (b *storageBlobs) verify(ctx context.Context) error {
	exists, err := b.client.BucketExists(ctx, b.bucket)
	if err != nil || !exists {
		return errors.New("dedicated product blob bucket is unavailable")
	}
	return nil
}
func (b *storageBlobs) put(ctx context.Context, key, contentType string, data []byte) error {
	return b.putReader(ctx, key, contentType, bytes.NewReader(data), int64(len(data)))
}
func (b *storageBlobs) putReader(ctx context.Context, key, contentType string, source io.Reader, size int64) error {
	if size < 0 || size > storageObjectLimit {
		return errors.New("invalid object upload size")
	}
	options := minio.PutObjectOptions{ContentType: contentType, DisableMultipart: true}
	allocation := ""
	if b.gcJournal != nil {
		var err error
		allocation, err = b.gcJournal.begin(ctx, key, size)
		if err != nil {
			return err
		}
		options.SetMatchETagExcept("*")
		options.UserMetadata = map[string]string{storageGCMetadata: allocation}
	}
	info, err := b.client.PutObject(ctx, b.bucket, key, source, size, options)
	canPublish := err == nil && info.Size == size
	if allocation != "" {
		// Finish with the same bounded request context. A canceled/unknown
		// journal commit retains uploading; it is never inferred to be an orphan.
		if journalErr := b.gcJournal.finish(ctx, key, allocation, canPublish); journalErr != nil {
			return journalErr
		}
	}
	if !canPublish {
		return errors.New("product blob upload failed or acknowledgment size differs")
	}
	return nil
}
func (b *storageBlobs) readPayload(ctx context.Context, key, digest string, size int64) (*storagePayload, error) {
	if size < 0 || size > storageObjectLimit || !storageSHA256.MatchString(digest) {
		return nil, errors.New("invalid stored object identity")
	}
	object, err := b.client.GetObject(ctx, b.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, errors.New("product blob read failed")
	}
	defer object.Close()
	p, err := readStoragePayload(ctx, object, size)
	if err != nil {
		return nil, err
	}
	if p.Size != size || p.SHA256 != digest {
		p.Close()
		return nil, errors.New("product blob integrity verification failed")
	}
	return p, nil
}
func (b *storageBlobs) read(ctx context.Context, key, digest string, size int64) ([]byte, error) {
	if size < 0 || size > storagePartLimit {
		return nil, errors.New("invalid stored object size")
	}
	object, err := b.client.GetObject(ctx, b.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, errors.New("product blob read failed")
	}
	defer object.Close()
	data, err := io.ReadAll(io.LimitReader(object, storagePartLimit+1))
	if err != nil || int64(len(data)) != size || fmt.Sprintf("%x", sha256.Sum256(data)) != digest {
		return nil, errors.New("product blob integrity verification failed")
	}
	return data, nil
}
