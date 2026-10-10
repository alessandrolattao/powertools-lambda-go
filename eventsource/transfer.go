package eventsource

import (
	"encoding/json/v2"
	"errors"
	"fmt"
)

// ErrInvalidHomeDirectory reports a Transfer Family response without the
// home directory its type needs.
var ErrInvalidHomeDirectory = errors.New("invalid home directory")

// TransferFamilyAuthorizerEvent is what AWS Transfer Family sends to a
// custom identity provider function.
type TransferFamilyAuthorizerEvent struct {
	Username string `json:"username"`
	// Password is nil for SSH public key authentication.
	Password *string `json:"password"`
	// Protocol is SFTP, FTP, FTPS or AS2.
	Protocol string `json:"protocol"`
	ServerID string `json:"serverId"`
	SourceIP string `json:"sourceIp"`
}

// The home directory types of a Transfer Family user.
const (
	TransferFamilyHomeDirectoryPath    = "PATH"
	TransferFamilyHomeDirectoryLogical = "LOGICAL"
)

// TransferFamilyAuthorizerResponse is what a custom identity provider
// function returns to authenticate a user. An empty response denies access.
type TransferFamilyAuthorizerResponse struct {
	Role              string `json:"Role"`
	HomeDirectoryType string `json:"HomeDirectoryType"`
	HomeDirectory     string `json:"HomeDirectory,omitempty"`
	// HomeDirectoryDetails is the JSON-encoded list of logical directory
	// mappings, as Transfer Family expects it.
	HomeDirectoryDetails string                      `json:"HomeDirectoryDetails,omitempty"`
	PosixProfile         *TransferFamilyPosixProfile `json:"PosixProfile,omitempty"`
	Policy               string                      `json:"Policy,omitempty"`
	PublicKeys           []string                    `json:"PublicKeys,omitempty"`
}

// TransferFamilyPosixProfile is the POSIX identity of an Amazon EFS user.
type TransferFamilyPosixProfile struct {
	UID int64 `json:"Uid"`
	GID int64 `json:"Gid"`
}

// TransferFamilyDirectoryMapping maps a path the user sees (Entry) to a path
// in the bucket or file system (Target).
type TransferFamilyDirectoryMapping struct {
	Entry  string `json:"Entry"`
	Target string `json:"Target"`
}

// TransferFamilyOption configures a Transfer Family response.
type TransferFamilyOption func(*transferFamilyResponse)

type transferFamilyResponse struct {
	response TransferFamilyAuthorizerResponse
	mappings []TransferFamilyDirectoryMapping
}

// WithHomeDirectory gives the user a PATH home directory.
func WithHomeDirectory(path string) TransferFamilyOption {
	return func(r *transferFamilyResponse) {
		r.response.HomeDirectoryType = TransferFamilyHomeDirectoryPath
		r.response.HomeDirectory = path
	}
}

// WithLogicalHomeDirectory gives the user a LOGICAL home directory made of
// mappings.
func WithLogicalHomeDirectory(mappings ...TransferFamilyDirectoryMapping) TransferFamilyOption {
	return func(r *transferFamilyResponse) {
		r.response.HomeDirectoryType = TransferFamilyHomeDirectoryLogical
		r.mappings = mappings
	}
}

// WithSessionPolicy scopes the user's role down with a session policy (a
// JSON IAM policy document).
func WithSessionPolicy(policy string) TransferFamilyOption {
	return func(r *transferFamilyResponse) { r.response.Policy = policy }
}

// WithPublicKeys lists the SSH public keys the user may authenticate with.
func WithPublicKeys(keys ...string) TransferFamilyOption {
	return func(r *transferFamilyResponse) { r.response.PublicKeys = keys }
}

// NewTransferFamilyS3Response authenticates a user of an Amazon S3 server
// with role. A home directory option is required.
func NewTransferFamilyS3Response(role string, opts ...TransferFamilyOption) (TransferFamilyAuthorizerResponse, error) {
	return buildTransferFamilyResponse(role, nil, opts)
}

// NewTransferFamilyEFSResponse authenticates a user of an Amazon EFS server
// with role and the POSIX identity uid:gid. A home directory option is
// required. Powertools Python writes the gid as the uid; this does not.
func NewTransferFamilyEFSResponse(role string, uid, gid int64, opts ...TransferFamilyOption) (TransferFamilyAuthorizerResponse, error) {
	return buildTransferFamilyResponse(role, &TransferFamilyPosixProfile{UID: uid, GID: gid}, opts)
}

func buildTransferFamilyResponse(role string, posix *TransferFamilyPosixProfile, opts []TransferFamilyOption) (TransferFamilyAuthorizerResponse, error) {
	r := transferFamilyResponse{response: TransferFamilyAuthorizerResponse{
		Role: role, HomeDirectoryType: TransferFamilyHomeDirectoryPath, PosixProfile: posix,
	}}
	for _, opt := range opts {
		opt(&r)
	}
	switch r.response.HomeDirectoryType {
	case TransferFamilyHomeDirectoryLogical:
		if len(r.mappings) == 0 {
			return TransferFamilyAuthorizerResponse{}, fmt.Errorf("%w: a LOGICAL home directory needs mappings", ErrInvalidHomeDirectory)
		}
		details, err := json.Marshal(r.mappings)
		if err != nil {
			return TransferFamilyAuthorizerResponse{}, fmt.Errorf("encode the home directory mappings: %w", err)
		}
		r.response.HomeDirectoryDetails = string(details)
	default:
		if r.response.HomeDirectory == "" {
			return TransferFamilyAuthorizerResponse{}, fmt.Errorf("%w: a PATH home directory needs a path", ErrInvalidHomeDirectory)
		}
	}
	return r.response, nil
}
