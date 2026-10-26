package control

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const storageGCMetadata = "Neon-Gc-Allocation"

var storageGCBlobKey = regexp.MustCompile(`^prj_[a-f0-9]{16,32}/(blob|part)_[a-f0-9]{24}$`)
var errStorageGCJournal = errors.New("product blob allocation evidence unavailable")

// No branch list or clock proves that published bytes have no historical
// references. may_reference is deliberately terminal until a separately trusted
// native all-timeline proof adapter exists. Crashes leave uploading held forever.
type storageGCJournal struct {
	db       *pgxpool.Pool
	writerDB *pgxpool.Pool
	storeID  string
	deleter  *storageGCDeleter
}

func (j *storageGCJournal) allocationDB() *pgxpool.Pool {
	if j.writerDB != nil {
		return j.writerDB
	}
	return j.db
}

func storageGCStoreID(endpoint, bucket string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte("neon-product-gc-store-v1\x00"+endpoint+"\x00"+bucket)))
}

func (j *storageGCJournal) begin(ctx context.Context, key string, size int64) (string, error) {
	if j == nil || j.db == nil || !storageGCBlobKey.MatchString(key) || size < 0 || size > storageObjectFormatLimit {
		return "", errStorageGCJournal
	}
	id := newID("sga_")
	_, err := j.allocationDB().Exec(ctx, `INSERT INTO storage_gc_allocations(store_id,blob_key,allocation_id,project_id,size) VALUES($1,$2,$3,$4,$5)`, j.storeID, key, id, strings.SplitN(key, "/", 2)[0], size)
	if err != nil {
		// An unknown INSERT result cannot authorize a backing write. Retrying a
		// key is refused even after a successful or interrupted physical delete.
		return "", errStorageGCJournal
	}
	return id, nil
}

func (j *storageGCJournal) finish(ctx context.Context, key, id string, canPublish bool) error {
	state := "never_published"
	if canPublish {
		state = "may_reference"
	}
	tag, err := j.allocationDB().Exec(ctx, `UPDATE storage_gc_allocations SET state=$4,resolved_at=clock_timestamp() WHERE store_id=$1 AND blob_key=$2 AND allocation_id=$3 AND state='uploading'`, j.storeID, key, id, state)
	if err != nil || tag.RowsAffected() != 1 {
		return errStorageGCJournal
	}
	return nil
}

// Called once after loadStorageBlobs, before serving API/Worker requests. Merely
// enabling the journal adds no DELETE capability to the ordinary blob client.
func (s *server) configureStorageGC() error {
	if s.storage == nil {
		return nil
	}
	if s.db == nil || s.storage.client == nil {
		return errStorageGCJournal
	}
	j := &storageGCJournal{db: s.db, storeID: storageGCStoreID(s.storage.client.EndpointURL().String(), s.storage.bucket)}
	d, err := loadStorageGCDeleter(s.storage)
	if err != nil {
		return err
	}
	j.deleter = d
	// Every admitted storage operation already holds a metadata connection.
	// Reserve independent bounded journal capacity so metadata pool saturation
	// cannot deadlock all request holders waiting for their allocation commit.
	cfg := s.db.Config()
	cfg.MinConns, cfg.MaxConns = 0, 2
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	j.writerDB, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return errStorageGCJournal
	}
	s.storage.gcJournal = j
	return nil
}

func (s *server) closeStorageGC() {
	if s.storage != nil && s.storage.gcJournal != nil && s.storage.gcJournal.writerDB != nil {
		s.storage.gcJournal.writerDB.Close()
	}
}
