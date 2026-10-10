package circuitbreaker_test

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// FuzzDynamoDBRecord feeds arbitrary items to the store: whatever another
// writer left in the table, reading it gives a valid record or an error,
// never a panic.
func FuzzDynamoDBRecord(f *testing.F) {
	f.Add("OPEN", "5", "1699999999", "42#a", "1700000030.5", true)
	f.Add("HALF_OPEN", "0", "-1", "", "1e300", false)
	f.Add("CLOSED", "x", "", "", "NaN", true)
	f.Fuzz(func(t *testing.T, state, count, openedAt, owner, lease string, stateIsString bool) {
		item := map[string]types.AttributeValue{"id": s("c")}
		if stateIsString {
			item["state"] = s(state)
		} else {
			item["state"] = n(state)
		}
		for attr, value := range map[string]string{"failure_count": count, "opened_at": openedAt, "probe_lease_expiry": lease} {
			if value != "" {
				item[attr] = n(value)
			}
		}
		if owner != "" {
			item["half_open_owner"] = s(owner)
		}
		record, found, err := newStore(t, &fakeDynamoDB{item: item}).Get(t.Context(), "c")
		if err != nil {
			return
		}
		if !found || !record.State.Valid() {
			t.Fatalf("Get accepted %v as %+v", item, record)
		}
	})
}
