package kms

import (
	"context"
	"fmt"
	"sync"

	mpltypes "github.com/aws/aws-cryptographic-material-providers-library/releases/go/mpl/awscryptographymaterialproviderssmithygeneratedtypes"
	"github.com/aws/smithy-go/middleware"
)

// dafnyMu serializes the parts of the AWS Encryption SDK and of the
// Material Providers Library that are not safe for concurrent use.
//
// Both are generated from Dafny, and their runtime (DafnyRuntimeGo v4.11.3)
// flags the shared empty sequence as a string every time a keyring or a
// materials manager builds one, from whatever goroutine runs it: a data race
// on a package variable of the runtime. It writes the same value each time,
// so it is harmless in practice, but the Go memory model promises nothing
// for it and the race detector reports it. Message parsing, AES-GCM and
// signatures run concurrently without it, so the lock covers the materials
// managers only (keyrings run inside them), and the KMS clients the package
// builds release it while a request is on the network.
var dafnyMu sync.Mutex

// lockedCMM runs a materials manager under dafnyMu.
type lockedCMM struct {
	inner mpltypes.ICryptographicMaterialsManager
}

func (c lockedCMM) GetEncryptionMaterials(in mpltypes.GetEncryptionMaterialsInput) (*mpltypes.GetEncryptionMaterialsOutput, error) {
	dafnyMu.Lock()
	defer dafnyMu.Unlock()
	return c.inner.GetEncryptionMaterials(in) //nolint:wrapcheck // the SDK classifies the errors of its own materials manager
}

func (c lockedCMM) DecryptMaterials(in mpltypes.DecryptMaterialsInput) (*mpltypes.DecryptMaterialsOutput, error) {
	dafnyMu.Lock()
	defer dafnyMu.Unlock()
	return c.inner.DecryptMaterials(in) //nolint:wrapcheck // the SDK classifies the errors of its own materials manager
}

// withDafnyMu runs f under dafnyMu: the constructors of the libraries run
// the same generated code.
func withDafnyMu[T any](f func() (T, error)) (T, error) {
	dafnyMu.Lock()
	defer dafnyMu.Unlock()
	return f()
}

// releaseDuringCall is a middleware for the KMS clients of the package's
// keyrings. Those keyrings only run inside a lockedCMM, so the lock is held
// when a request starts; releasing it until the response is back lets other
// invocations encrypt and decrypt while KMS answers this one.
func releaseDuringCall(stack *middleware.Stack) error {
	err := stack.Initialize.Add(middleware.InitializeMiddlewareFunc("PowertoolsDataMaskingReleaseLock",
		func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
			middleware.InitializeOutput, middleware.Metadata, error,
		) {
			if dafnyMu.TryLock() {
				// Called outside a materials manager, which the package never
				// does: there is nothing to release.
				dafnyMu.Unlock()
				return next.HandleInitialize(ctx, in)
			}
			dafnyMu.Unlock()
			defer dafnyMu.Lock()
			return next.HandleInitialize(ctx, in)
		}), middleware.Before)
	if err != nil {
		return fmt.Errorf("datamasking/kms: add the lock middleware: %w", err)
	}
	return nil
}
