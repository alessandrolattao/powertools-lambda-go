package dynamodb_test

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency/dynamodb"
)

// FuzzGetRecord reads items with arbitrary attribute values, as a table
// shared with other writers may hold: the store returns a record or an
// error, never panics.
func FuzzGetRecord(f *testing.F) {
	f.Add("COMPLETED", "1636549553", "1636549553123", `{"a":1}`, true)
	f.Add("", "", "", "", false)
	f.Add("INPROGRESS", "1.5e3", "-1", "", true)
	f.Fuzz(func(t *testing.T, status, expiry, inProgressExpiry, data string, numbers bool) {
		fake := newFakeDynamoDB("id")
		item := map[string]types.AttributeValue{"id": s("k"), "status": s(status), "data": s(data)}
		if numbers {
			item["expiration"] = n(expiry)
			item["in_progress_expiration"] = n(inProgressExpiry)
		} else {
			item["expiration"] = s(expiry)
		}
		fake.items["k"] = item
		store, err := dynamodb.New(fake, "t")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = store.GetRecord(t.Context(), "k") //nolint:errcheck // the property is the absence of panics
	})
}
