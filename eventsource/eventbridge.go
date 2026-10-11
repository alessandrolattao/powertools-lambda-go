package eventsource

import (
	"github.com/aws/aws-lambda-go/events"
)

// EventBridgeEvent is events.EventBridgeEvent with the name of the replay
// that sent it, when it comes from an archive replay. For a typed detail,
// see the parser package.
type EventBridgeEvent struct {
	events.EventBridgeEvent

	ReplayName string `json:"replay-name,omitempty"`
}

// S3EventBridgeNotificationEvent is an Amazon S3 event delivered through
// EventBridge (Object Created, Object Deleted, Object Restore Completed...).
type S3EventBridgeNotificationEvent struct {
	EventBridgeEvent

	Detail S3EventBridgeNotificationDetail `json:"detail"`
}

// S3EventBridgeNotificationDetail is the detail of an S3 event delivered
// through EventBridge. Which of the optional members are present depends on
// the detail type.
type S3EventBridgeNotificationDetail struct {
	Version   string                          `json:"version"`
	Bucket    S3EventBridgeNotificationBucket `json:"bucket"`
	Object    S3EventBridgeNotificationObject `json:"object"`
	RequestID string                          `json:"request-id"`
	Requester string                          `json:"requester"`

	SourceIPAddress         string `json:"source-ip-address,omitempty"`
	Reason                  string `json:"reason,omitempty"`
	DeletionType            string `json:"deletion-type,omitempty"`
	RestoreExpiryTime       string `json:"restore-expiry-time,omitempty"`
	SourceStorageClass      string `json:"source-storage-class,omitempty"`
	DestinationStorageClass string `json:"destination-storage-class,omitempty"`
	DestinationAccessTier   string `json:"destination-access-tier,omitempty"`
}

// S3EventBridgeNotificationBucket is the bucket of the event.
type S3EventBridgeNotificationBucket struct {
	Name string `json:"name"`
}

// S3EventBridgeNotificationObject is the object of the event.
type S3EventBridgeNotificationObject struct {
	// Key is URL-encoded as S3 sends it; URLDecodedKey decodes it.
	Key  string `json:"key"`
	Size int64  `json:"size,omitempty"`
	// ETag is empty for deletions.
	ETag string `json:"etag,omitempty"`
	// VersionID is empty for buckets without versioning (Powertools Python
	// wrongly requires it).
	VersionID string `json:"version-id,omitempty"`
	Sequencer string `json:"sequencer"`
}

// URLDecodedKey returns the object key decoded the way Python's
// urllib.parse.unquote_plus does: "+" is a space, %XX escapes are decoded,
// malformed escapes are left as they are.
func (o S3EventBridgeNotificationObject) URLDecodedKey() string {
	return UnquoteS3Key(o.Key)
}
