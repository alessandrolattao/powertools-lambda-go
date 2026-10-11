package metadata

import (
	"testing"
)

// FuzzDecode feeds response bodies to the decoder: whatever the endpoint
// answers, decoding fails with an error or keeps the body, never panics.
func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{"AvailabilityZoneID":"use1-az1"}`))
	f.Add([]byte(`{"AvailabilityZoneID":"use1-az1","Future":[1,{"a":null}]}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"AvailabilityZoneID":1}`))
	f.Add([]byte(`{"AvailabilityZoneID":"a","AvailabilityZoneID":"b"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		md, err := decode(body)
		if err != nil {
			if md.AvailabilityZoneID != "" || md.Raw != nil {
				t.Fatalf("a failed decode returned %+v", md)
			}
			return
		}
		if md.Raw.Kind() != '{' {
			t.Fatalf("decoded a %v", md.Raw.Kind())
		}
	})
}
