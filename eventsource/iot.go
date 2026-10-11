package eventsource

import (
	"time"
)

// IoTRegistryEvent holds what every AWS IoT Core registry event has.
type IoTRegistryEvent struct {
	EventID   string `json:"eventId"`
	EventType string `json:"eventType"`
	// Timestamp is in milliseconds (seconds in some older events); Time
	// reads either.
	Timestamp int64  `json:"timestamp"`
	Operation string `json:"operation"`
}

// Time returns the timestamp as a time.Time in UTC: a value above 10^10 is
// taken as milliseconds, a smaller one as seconds, as Powertools Python does
// (which returns local time instead).
func (e IoTRegistryEvent) Time() time.Time {
	if e.Timestamp > 1e10 {
		return time.UnixMilli(e.Timestamp).UTC()
	}
	return time.Unix(e.Timestamp, 0).UTC()
}

// The event types of the IoT Core registry events.
const (
	IoTThingEvent                = "THING_EVENT"
	IoTThingTypeEvent            = "THING_TYPE_EVENT"
	IoTThingTypeAssociationEvent = "THING_TYPE_ASSOCIATION_EVENT"
	IoTThingGroupEvent           = "THING_GROUP_EVENT"
	IoTThingGroupMembershipEvent = "THING_GROUP_MEMBERSHIP_EVENT"
	IoTThingGroupHierarchyEvent  = "THING_GROUP_HIERARCHY_EVENT"
)

// The operations of the IoT Core registry events.
const (
	IoTOperationCreated = "CREATED"
	IoTOperationUpdated = "UPDATED"
	IoTOperationDeleted = "DELETED"
	IoTOperationAdded   = "ADDED"
	IoTOperationRemoved = "REMOVED"
)

// IoTCoreThingEvent reports a thing created, updated or deleted.
type IoTCoreThingEvent struct {
	IoTRegistryEvent

	AccountID     string         `json:"accountId"`
	ThingID       string         `json:"thingId"`
	ThingName     string         `json:"thingName"`
	VersionNumber int64          `json:"versionNumber"`
	ThingTypeName string         `json:"thingTypeName,omitempty"`
	Attributes    map[string]any `json:"attributes"`
}

// IoTCoreThingTypeEvent reports a thing type created, updated, deprecated,
// undeprecated or deleted.
type IoTCoreThingTypeEvent struct {
	IoTRegistryEvent

	AccountID     string `json:"accountId"`
	ThingTypeID   string `json:"thingTypeId"`
	ThingTypeName string `json:"thingTypeName"`
	IsDeprecated  bool   `json:"isDeprecated"`
	// DeprecationDate is an ISO 8601 time, empty unless deprecated.
	DeprecationDate       string              `json:"deprecationDate,omitempty"`
	SearchableAttributes  []string            `json:"searchableAttributes"`
	PropagatingAttributes []map[string]string `json:"propagatingAttributes"`
	Description           string              `json:"description"`
}

// IoTCoreThingTypeAssociationEvent reports a thing associated with or
// dissociated from a thing type.
type IoTCoreThingTypeAssociationEvent struct {
	IoTRegistryEvent

	ThingID       string `json:"thingId"`
	ThingName     string `json:"thingName"`
	ThingTypeName string `json:"thingTypeName"`
}

// IoTCoreThingGroupEvent reports a thing group created, updated or deleted.
type IoTCoreThingGroupEvent struct {
	IoTRegistryEvent

	AccountID               string              `json:"accountId"`
	ThingGroupID            string              `json:"thingGroupId"`
	ThingGroupName          string              `json:"thingGroupName"`
	VersionNumber           int64               `json:"versionNumber"`
	ParentGroupName         string              `json:"parentGroupName,omitempty"`
	ParentGroupID           string              `json:"parentGroupId,omitempty"`
	Description             string              `json:"description"`
	RootToParentThingGroups []map[string]string `json:"rootToParentThingGroups"`
	Attributes              map[string]any      `json:"attributes"`
	DynamicGroupMappingID   string              `json:"dynamicGroupMappingId,omitempty"`
}

// IoTCoreAddOrRemoveFromThingGroupEvent reports a thing added to or removed
// from a thing group.
type IoTCoreAddOrRemoveFromThingGroupEvent struct {
	IoTRegistryEvent

	AccountID    string `json:"accountId"`
	GroupARN     string `json:"groupArn"`
	GroupID      string `json:"groupId"`
	ThingARN     string `json:"thingArn"`
	ThingID      string `json:"thingId"`
	MembershipID string `json:"membershipId"`
}

// IoTCoreAddOrDeleteFromThingGroupEvent reports a child thing group added
// to or removed from a parent group.
type IoTCoreAddOrDeleteFromThingGroupEvent struct {
	IoTRegistryEvent

	AccountID      string `json:"accountId"`
	ThingGroupID   string `json:"thingGroupId"`
	ThingGroupName string `json:"thingGroupName"`
	ChildGroupID   string `json:"childGroupId"`
	ChildGroupName string `json:"childGroupName"`
}
