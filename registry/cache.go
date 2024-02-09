package registry

import (
	"context"
	"io"
	"sync"

	"cuelabs.dev/go/oci/ociregistry"
	"cuelabs.dev/go/oci/ociregistry/ocimem"
)

// https://github.com/opencontainers/distribution-spec/pull/293#issuecomment-1452780554
// TODO this should probably be submitted as a "const" in the image-spec ("ocispec.SuggestedManifestSizeLimit" or somesuch)
const manifestSizeLimit = 4 * 1024 * 1024

// this implements a transparent in-memory cache on top of objects less than 4MiB in size from the given registry -- it (currently) assumes a short lifecycle, not a long-running program, so use with care!
//
// TODO options (so we can control *what* gets cached, such as our size limit, whether to cache tag lookups, whether cached data should have a TTL, etc; see manifestSizeLimit and getBlob)
func RegistryCache(r ociregistry.Interface) ociregistry.Interface {
	return &registryCache{
		registry: r, // TODO support "nil" here so this can be a poor-man's ocimem implementation? 👀  see also https://github.com/cue-labs/oci/issues/24
		has:      map[string]bool{},
		tags:     map[string]ociregistry.Digest{},
		types:    map[ociregistry.Digest]string{},
		data:     map[ociregistry.Digest][]byte{},
	}
}

type registryCache struct {
	*ociregistry.Funcs

	registry ociregistry.Interface

	// https://github.com/cue-labs/oci/issues/24
	mu    sync.Mutex                    // TODO some kind of per-object/name/digest mutex so we don't request the same object from the upstream registry concurrently (on *top* of our maps mutex)?
	has   map[string]bool               // "repo/name@digest" => true (whether a given repo has the given digest)
	tags  map[string]ociregistry.Digest // "repo/name:tag" => digest
	types map[ociregistry.Digest]string // digest => "mediaType" (most recent *storing* / "cache-miss" lookup wins, in the case of upstream/cross-repo ambiguity)
	data  map[ociregistry.Digest][]byte // digest => data
}

func cacheKeyDigest(repo string, digest ociregistry.Digest) string {
	return repo + "@" + digest.String()
}

func cacheKeyTag(repo, tag string) string {
	return repo + ":" + tag
}

// a helper that implements GetBlob and GetManifest generically (since they're the same function signature and it doesn't really help *us* to treat those object types differently here)
func (rc *registryCache) getBlob(ctx context.Context, repo string, digest ociregistry.Digest, f func(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error)) (ociregistry.BlobReader, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if b, ok := rc.data[digest]; ok && rc.has[cacheKeyDigest(repo, digest)] {
		return ocimem.NewBytesReader(b, ociregistry.Descriptor{
			MediaType: rc.types[digest],
			Digest:    digest,
			Size:      int64(len(b)),
		}), nil
	}

	r, err := f(ctx, repo, digest)
	if err != nil {
		return nil, err
	}
	//defer r.Close()

	desc := r.Descriptor()

	rc.has[cacheKeyDigest(repo, desc.Digest)] = true
	rc.types[desc.Digest] = desc.MediaType

	b, err := io.ReadAll(r)
	if err != nil {
		r.Close()
		return nil, err
	}
	if err := r.Close(); err != nil {
		return nil, err
	}

	if len(b) <= manifestSizeLimit {
		rc.data[desc.Digest] = b
	} else {
		delete(rc.data, desc.Digest)
	}

	return ocimem.NewBytesReader(b, desc), nil
}

func (rc *registryCache) GetBlob(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	return rc.getBlob(ctx, repo, digest, rc.registry.GetBlob)
}

func (rc *registryCache) GetManifest(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	return rc.getBlob(ctx, repo, digest, rc.registry.GetManifest)
}

func (rc *registryCache) GetTag(ctx context.Context, repo string, tag string) (ociregistry.BlobReader, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	tagKey := cacheKeyTag(repo, tag)

	if digest, ok := rc.tags[tagKey]; ok {
		if b, ok := rc.data[digest]; ok {
			return ocimem.NewBytesReader(b, ociregistry.Descriptor{
				MediaType: rc.types[digest],
				Digest:    digest,
				Size:      int64(len(b)),
			}), nil
		}
	}

	r, err := rc.registry.GetTag(ctx, repo, tag)
	if err != nil {
		return nil, err
	}
	//defer r.Close()

	desc := r.Descriptor()

	rc.has[cacheKeyDigest(repo, desc.Digest)] = true
	rc.tags[tagKey] = desc.Digest
	rc.types[desc.Digest] = desc.MediaType

	b, err := io.ReadAll(r)
	if err != nil {
		r.Close()
		return nil, err
	}
	if err := r.Close(); err != nil {
		return nil, err
	}

	if len(b) <= manifestSizeLimit {
		rc.data[desc.Digest] = b
	} else {
		delete(rc.data, desc.Digest)
	}

	return ocimem.NewBytesReader(b, desc), nil
}

// TODO more methods (currently only implements what's actually necessary for SynthesizeIndex)
