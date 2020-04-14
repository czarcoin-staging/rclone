// Package storj provides an interface to Storj object storage.
package storj

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/bucket"
	"golang.org/x/text/unicode/norm"

	"storj.io/uplink"
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "storj",
		Description: "Storj Connection",
		NewFs:       NewFs,
		Options: []fs.Option{
			{
				Name:     "access",
				Help:     "Uplink access.",
				Required: false,
			},
			{
				Name:     "satellite-address",
				Help:     "Satellite address.",
				Required: false,
			},
			{
				Name:     "api-key",
				Help:     "API key.",
				Required: false,
			},
			{
				Name:     "passphrase",
				Help:     "Encryption passphrase.",
				Required: false,
			},
		},
	})
}

// Options defines the configuration for this backend
type Options struct {
	Access string `config:"access"`

	SatelliteAddress string `config:"satellite-address"`
	ApiKey           string `config:"api-key"`
	Passphrase       string `config:"passphrase"`
}

// Fs represents a remote storj server
type Fs struct {
	name string // the name of the remote
	root string // root of the filesystem

	object *Object // non-nil if the root is an object itself

	opts     Options      // parsed options
	features *fs.Features // optional features

	access *uplink.Access // parsed scope

	project *uplink.Project // project client
}

// Check the interfaces are satisfied.
var (
	_ fs.Fs          = &Fs{}
	_ fs.ListRer     = &Fs{}
	_ fs.PutStreamer = &Fs{}
)

// NewFs creates a filesystem backed by Storj.
func NewFs(name, root string, m configmap.Mapper) (_ fs.Fs, err error) {
	ctx := context.Background()

	f := &Fs{
		name: name,
		root: root,
	}

	// Parse config into Options struct
	err = configstruct.Set(m, &f.opts)
	if err != nil {
		return nil, err
	}

	// Parse access
	var access *uplink.Access

	if f.opts.Access != "" {
		access, err = uplink.ParseAccess(f.opts.Access)
		if err != nil {
			return nil, errors.Wrap(err, "storj: access")
		}
	}

	if access == nil && f.opts.SatelliteAddress != "" && f.opts.ApiKey != "" && f.opts.Passphrase != "" {
		access, err = uplink.RequestAccessWithPassphrase(ctx, f.opts.SatelliteAddress, f.opts.ApiKey, f.opts.Passphrase)
		if err != nil {
			return nil, errors.Wrap(err, "storj: access")
		}

		serializedAccess, err := access.Serialize()
		if err != nil {
			return nil, errors.Wrap(err, "storj: access")
		}

		err = config.SetValueAndSave(f.name, "access", serializedAccess)
		if err != nil {
			return nil, errors.Wrap(err, "storj: access")
		}
	}

	if access == nil {
		return nil, errors.New("access not found")
	}

	f.access = access

	// Setup filesystem and connection to Storj
	root = norm.NFC.String(root)
	root = strings.Trim(root, "/")

	f.features = (&fs.Features{
		BucketBased:       true,
		BucketBasedRootOK: true,
	}).Fill(f)

	project, err := f.connect(ctx)
	if err != nil {
		return nil, err
	}
	f.project = project

	// Root validation needs to check the following: If a bucket path is
	// specified and exists, then the object must be a directory.
	//
	// NOTE: At this point this must return the filesystem object we've
	// created so far even if there is an error.
	if root != "" {
		bucketName, bucketPath := bucket.Split(root)

		if bucketName != "" && bucketPath != "" {
			_, err = project.StatBucket(ctx, bucketName)
			if err != nil {
				return f, errors.Wrap(err, "storj: bucket")
			}

			object, err := project.StatObject(ctx, bucketName, bucketPath)
			if err == nil {
				if !object.IsPrefix {
					f.object = NewObjectFromUplink(f, "", object)

					return f, fs.ErrorIsFile
				}
			}
		}
	}

	return f, nil
}

// connect opens a connection to Storj.
func (f *Fs) connect(ctx context.Context) (project *uplink.Project, err error) {
	fs.Debugf(f, "connecting...")
	defer fs.Debugf(f, "connected: %+v", err)

	cfg := uplink.Config{}

	project, err = cfg.OpenProject(ctx, f.access)
	if err != nil {
		return nil, errors.Wrap(err, "storj: project")
	}

	return
}

// absolute computes the absolute bucket name and path from the filesystem root
// and the relative path provided.
func (f *Fs) absolute(relative string) (bucketName, bucketPath string) {
	if f.object != nil {
		return bucket.Split(f.root)
	}

	bn, bp := bucket.Split(path.Join(f.root, relative))

	// NOTE: Technically libuplink does not care about the encoding. It is
	// happy to work with them as opaque byte sequences. However, rclone
	// has a test that requires two paths with the same normalized form
	// (but different un-normalized forms) to point to the same file. This
	// means we have to normalize before we interact with libuplink.
	return norm.NFC.String(bn), norm.NFC.String(bp)
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String returns a description of the FS
func (f *Fs) String() string {
	return fmt.Sprintf("FS sj://%s", f.root)
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return time.Nanosecond
}

// Hashes returns the supported hash types of the filesystem.
func (f *Fs) Hashes() hash.Set {
	return hash.NewHashSet()
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// List the objects and directories in relative into entries. The entries can
// be returned in any order but should be for a complete directory.
//
// relative should be "" to list the root, and should not have trailing
// slashes.
//
// This should return fs.ErrDirNotFound if the directory isn't found.
func (f *Fs) List(ctx context.Context, relative string) (entries fs.DirEntries, err error) {
	fs.Debugf(f, "ls ./%s", relative)

	bucketName, bucketPath := f.absolute(relative)

	if bucketName == "" {
		if bucketPath != "" {
			return nil, fs.ErrorListBucketRequired
		}

		return f.listBuckets(ctx)
	}

	return f.listObjects(ctx, relative, bucketName, bucketPath)
}

func (f *Fs) listBuckets(ctx context.Context) (entries fs.DirEntries, err error) {
	fs.Debugf(f, "BKT ls")

	buckets := f.project.ListBuckets(ctx, nil)

	for buckets.Next() {
		bucket := buckets.Item()

		entries = append(entries, fs.NewDir(bucket.Name, bucket.Created))
	}

	return entries, buckets.Err()
}

// newDirEntry creates a directory entry from an uplink object.
//
// NOTE: Getting the exact behavior required by rclone is somewhat tricky. The
// path manipulation here is necessary to cover all the different ways the
// filesystem and object could be initialized and combined.
func (f *Fs) newDirEntry(relative, prefix string, object *uplink.Object) fs.DirEntry {
	if object.IsPrefix {
		//                         . The entry must include the relative path as its prefix. Depending on
		//                         | what is being listed and how the filesystem root was initialized the
		//                         | relative path may be empty (and so we use path joining here to ensure
		//                         | we don't end up with an empty path segment).
		//                         |
		//                         |                    . Remove the prefix used during listing.
		//                         |                    |
		//                         |                    |           . Remove the trailing slash.
		//                         |                    |           |
		//                         v                    v           v
		return fs.NewDir(path.Join(relative, object.Key[len(prefix):len(object.Key)-1]), object.System.Created)
	}

	return NewObjectFromUplink(f, relative, object)
}

func (f *Fs) listObjects(ctx context.Context, relative, bucketName, bucketPath string) (entries fs.DirEntries, err error) {
	fs.Debugf(f, "OBJ ls ./%s (%q, %q)", relative, bucketName, bucketPath)

	opts := &uplink.ListObjectsOptions{
		Prefix: newPrefix(bucketPath),

		System: true,
		Custom: true,
	}
	fs.Debugf(f, "opts %+v", opts)

	objects := f.project.ListObjects(ctx, bucketName, opts)

	for objects.Next() {
		entries = append(entries, f.newDirEntry(relative, opts.Prefix, objects.Item()))
	}

	err = objects.Err()
	if err != nil {
		return nil, err
	}

	// NOTE: This is necessary because libuplink does not support listings
	// without a trailing slash `/`. This means you can't call list on an
	// actual object. However, rclone expects you to be able to do this. To
	// bridge the gap we do a stat on the base of the listing and add it to
	// our results if necessary.
	object, err := f.project.StatObject(ctx, bucketName, bucketPath)
	if err == nil {
		entries = append(entries, f.newDirEntry(relative, opts.Prefix, object))
	}

	return entries, nil
}

// ListR lists the objects and directories of the Fs starting from dir
// recursively into out.
//
// relative should be "" to start from the root, and should not have trailing
// slashes.
//
// This should return ErrDirNotFound if the directory isn't found.
//
// It should call callback for each tranche of entries read. These need not be
// returned in any particular order. If callback returns an error then the
// listing will stop immediately.
//
// Don't implement this unless you have a more efficient way of listing
// recursively that doing a directory traversal.
func (f *Fs) ListR(ctx context.Context, relative string, callback fs.ListRCallback) (err error) {
	fs.Debugf(f, "ls -R ./%s", relative)

	bucketName, bucketPath := f.absolute(relative)

	if bucketName == "" {
		if bucketPath != "" {
			return fs.ErrorListBucketRequired
		}

		return f.listBucketsR(ctx, callback)
	}

	return f.listObjectsR(ctx, relative, bucketName, bucketPath, callback)
}

func (f *Fs) listBucketsR(ctx context.Context, callback fs.ListRCallback) (err error) {
	fs.Debugf(f, "BKT ls -R")

	buckets := f.project.ListBuckets(ctx, nil)

	for buckets.Next() {
		bucket := buckets.Item()

		err = f.listObjectsR(ctx, bucket.Name, bucket.Name, "", callback)
		if err != nil {
			return err
		}
	}

	return buckets.Err()
}

func (f *Fs) listObjectsR(ctx context.Context, relative, bucketName, bucketPath string, callback fs.ListRCallback) (err error) {
	fs.Debugf(f, "OBJ ls -R ./%s (%q, %q)", relative, bucketName, bucketPath)

	opts := &uplink.ListObjectsOptions{
		Prefix:    newPrefix(bucketPath),
		Recursive: true,

		System: true,
		Custom: true,
	}

	objects := f.project.ListObjects(ctx, bucketName, opts)

	for objects.Next() {
		object := objects.Item()

		err = callback(fs.DirEntries{f.newDirEntry(relative, opts.Prefix, object)})
		if err != nil {
			return err
		}
	}

	err = objects.Err()
	if err != nil {
		return err
	}

	// NOTE: This is necessary because libuplink does not support listings
	// without a trailing slash `/`. This means you can't call list on an
	// actual object. However, rclone expects you to be able to do this. To
	// bridge the gap we do a stat on the base of the listing and add it to
	// our results if necessary.
	object, err := f.project.StatObject(ctx, bucketName, bucketPath)
	if err == nil {
		err = callback(fs.DirEntries{f.newDirEntry(relative, opts.Prefix, object)})
		if err != nil {
			return err
		}
	}

	return nil
}

// NewObject finds the Object at relative. If it can't be found it returns the
// error ErrorObjectNotFound.
func (f *Fs) NewObject(ctx context.Context, relative string) (_ fs.Object, err error) {
	fs.Debugf(f, "stat ./%s", relative)

	bucketName, bucketPath := f.absolute(relative)

	object, err := f.project.StatObject(ctx, bucketName, bucketPath)
	if err != nil {
		fs.Debugf(f, "err: %+v", err)

		return nil, fs.ErrorObjectNotFound
	}

	return NewObjectFromUplink(f, relative, object), nil
}

// Put in to the remote path with the modTime given of the given size
//
// When called from outside a Fs by rclone, src.Size() will always be >= 0.
// But for unknown-sized objects (indicated by src.Size() == -1), Put should
// either return an error or upload it properly (rather than e.g. calling
// panic).
//
// May create the object even if it returns an error - if so will return the
// object and the error, otherwise will return nil and the error
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (_ fs.Object, err error) {
	fs.Debugf(f, "cp input ./%s # %+v %d", src.Remote(), options, src.Size())

	// Reject options we don't support.
	for _, option := range options {
		switch opt := option.(type) {
		case *fs.RangeOption:
			fs.Errorf(f, "Unsupported range option: %v", opt)

			return nil, errors.New("ranged writes are not supported")
		case *fs.SeekOption:
			if opt.Offset != 0 {
				fs.Errorf(f, "Unsupported seek option: %v", opt)

				return nil, errors.New("writes from a non-zero offset are not supported")
			}
		default:
			if option.Mandatory() {
				fs.Errorf(f, "Unsupported mandatory option: %v", option)

				return nil, errors.New("unsupported mandatory option")
			}
		}
	}

	bucketName, bucketPath := f.absolute(src.Remote())

	upload, err := f.project.UploadObject(ctx, bucketName, bucketPath, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			aerr := upload.Abort()
			if aerr != nil {
				fs.Errorf(f, "cp input ./%s %+v: %+v", src.Remote(), options, aerr)
			}
		}
	}()

	err = upload.SetCustomMetadata(ctx, uplink.CustomMetadata{
		"rclone:mtime": src.ModTime(ctx).Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, err
	}

	_, err = io.Copy(upload, in)
	if err != nil {
		err = fserrors.RetryError(err)
		fs.Errorf(f, "cp input ./%s %+v: %+v\n", src.Remote(), options, err)

		return nil, err
	}

	err = upload.Commit()
	if err != nil {
		return nil, err
	}

	return NewObjectFromUplink(f, "", upload.Info()), nil
}

// PutStream uploads to the remote path with the modTime given of indeterminate
// size.
//
// May create the object even if it returns an error - if so will return the
// object and the error, otherwise will return nil and the error.
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (_ fs.Object, err error) {
	return f.Put(ctx, in, src, options...)
}

// Mkdir makes the directory (container, bucket)
//
// Shouldn't return an error if it already exists
func (f *Fs) Mkdir(ctx context.Context, relative string) (err error) {
	fs.Debugf(f, "mkdir -p ./%s", relative)

	bucketName, _ := f.absolute(relative)

	_, err = f.project.EnsureBucket(ctx, bucketName)

	return err
}

// Rmdir removes the directory (container, bucket)
//
// NOTE: Despite code documentation to the contrary, this method should not
// return an error if the directory does not exist.
func (f *Fs) Rmdir(ctx context.Context, relative string) (err error) {
	fs.Debugf(f, "rmdir ./%s", relative)

	bucketName, bucketPath := f.absolute(relative)

	if bucketPath != "" {
		// If we can successfully stat it, then it is an object (and not a prefix).
		_, err := f.project.StatObject(ctx, bucketName, bucketPath)
		if err != nil {
			if errors.Is(err, uplink.ErrObjectNotFound) {
				// At this point we know it is not an object,
				// but we don't know if it is a prefix for one.
				//
				// We check this by doing a listing and if we
				// get any results back, then we know this is a
				// valid prefix (which implies the directory is
				// not empty).
				opts := &uplink.ListObjectsOptions{
					Prefix: newPrefix(bucketPath),

					System: true,
					Custom: true,
				}

				objects := f.project.ListObjects(ctx, bucketName, opts)

				if objects.Next() {
					return fs.ErrorDirectoryNotEmpty
				}

				return objects.Err()
			}

			return err
		}

		return fs.ErrorIsFile
	}

	_, err = f.project.DeleteBucket(ctx, bucketName)
	if err != nil {
		if errors.Is(err, uplink.ErrBucketNotFound) {
			return fs.ErrorDirNotFound
		}

		if errors.Is(err, uplink.ErrBucketNotEmpty) {
			return fs.ErrorDirectoryNotEmpty
		}

		return err
	}

	return nil
}

// newPrefix returns a new prefix for listing conforming to the libuplink
// requirements. In particular, libuplink requires a trailing slash for
// listings, but rclone does not always provide one. Further, depending on how
// the path was initially path normalization may have removed it (e.g. a
// trailing slash from the CLI is removed before it ever get's to the backend
// code).
func newPrefix(prefix string) string {
	if prefix == "" {
		return prefix
	}

	if prefix[len(prefix)-1] == '/' {
		return prefix
	}

	return prefix + "/"
}
