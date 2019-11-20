// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package storage

import (
	"context"
	"io"
	"os"

	"github.com/zeebo/errs"
)

// ErrInvalidBlobRef is returned when an blob reference is invalid
var ErrInvalidBlobRef = errs.Class("invalid blob ref")

// FormatVersion represents differing storage format version values. Different Blobs implementors
// might interpret different FormatVersion values differently, but they share a type so that there
// can be a common StorageFormatVersion() call on the interface.
//
// Changes in FormatVersion might affect how a Blobs or BlobReader or BlobWriter instance works, or
// they might only be relevant to some higher layer. A FormatVersion must be specified when writing
// a new blob, and the blob storage interface must store that value with the blob somehow, so that
// the same FormatVersion is returned later when reading that stored blob.
type FormatVersion int

// BlobRef is a reference to a blob
type BlobRef struct {
	Namespace []byte
	Key       []byte
}

// IsValid returns whether both namespace and key are specified
func (ref *BlobRef) IsValid() bool {
	return len(ref.Namespace) > 0 && len(ref.Key) > 0
}

// BlobReader is an interface that groups Read, ReadAt, Seek and Close.
type BlobReader interface {
	io.Reader
	io.ReaderAt
	io.Seeker
	io.Closer
	// Size returns the size of the blob
	Size() (int64, error)
	// StorageFormatVersion returns the storage format version associated with the blob.
	StorageFormatVersion() FormatVersion
}

// BlobWriter defines the interface that must be satisfied for a general blob storage provider.
// BlobWriter instances are returned by the Create() method on Blobs instances.
type BlobWriter interface {
	io.Writer
	io.Seeker
	// Cancel discards the blob.
	Cancel(context.Context) error
	// Commit ensures that the blob is readable by others.
	Commit(context.Context) error
	// Size returns the size of the blob
	Size() (int64, error)
	// StorageFormatVersion returns the storage format version associated with the blob.
	StorageFormatVersion() FormatVersion
}

// Blobs is a blob storage interface.
//
// architecture: Database
type Blobs interface {
	// Create creates a new blob that can be written
	// optionally takes a size argument for performance improvements, -1 is unknown size
	Create(ctx context.Context, ref BlobRef, size int64) (BlobWriter, error)
	// Open opens a reader with the specified namespace and key
	Open(ctx context.Context, ref BlobRef) (BlobReader, error)
	// OpenWithStorageFormat opens a reader for the already-located blob, avoiding the potential
	// need to check multiple storage formats to find the blob.
	OpenWithStorageFormat(ctx context.Context, ref BlobRef, formatVer FormatVersion) (BlobReader, error)
	// Delete deletes the blob with the namespace and key
	Delete(ctx context.Context, ref BlobRef) error
	// DeleteWithStorageFormat deletes a blob of a specific storage format
	DeleteWithStorageFormat(ctx context.Context, ref BlobRef, formatVer FormatVersion) error
	// Trash marks a file for pending deletion
	Trash(ctx context.Context, ref BlobRef) error
	// RestoreTrash restores all files in the trash for a given namespace
	RestoreTrash(ctx context.Context, namespace []byte) error
	// Stat looks up disk metadata on the blob file
	Stat(ctx context.Context, ref BlobRef) (BlobInfo, error)
	// StatWithStorageFormat looks up disk metadata for the blob file with the given storage format
	// version. This avoids the potential need to check multiple storage formats for the blob
	// when the format is already known.
	StatWithStorageFormat(ctx context.Context, ref BlobRef, formatVer FormatVersion) (BlobInfo, error)
	// FreeSpace return how much free space left for writing
	FreeSpace() (int64, error)
	// SpaceUsed adds up how much is used in all namespaces
	SpaceUsed(ctx context.Context) (int64, error)
	// SpaceUsedInNamespace adds up how much is used in the given namespace
	SpaceUsedInNamespace(ctx context.Context, namespace []byte) (int64, error)
	// ListNamespaces finds all namespaces in which keys might currently be stored.
	ListNamespaces(ctx context.Context) ([][]byte, error)
	// WalkNamespace executes walkFunc for each locally stored blob, stored with
	// storage format V1 or greater, in the given namespace. If walkFunc returns a non-nil
	// error, WalkNamespace will stop iterating and return the error immediately. The ctx
	// parameter is intended to allow canceling iteration early.
	WalkNamespace(ctx context.Context, namespace []byte, walkFunc func(BlobInfo) error) error
	// Close closes the blob store and any resources associated with it.
	Close() error
}

// BlobInfo allows lazy inspection of a blob and its underlying file during iteration with
// WalkNamespace-type methods
type BlobInfo interface {
	// BlobRef returns the relevant BlobRef for the blob
	BlobRef() BlobRef
	// StorageFormatVersion indicates the storage format version used to store the piece
	StorageFormatVersion() FormatVersion
	// FullPath gives the full path to the on-disk blob file
	FullPath(ctx context.Context) (string, error)
	// Stat does a stat on the on-disk blob file
	Stat(ctx context.Context) (os.FileInfo, error)
}
