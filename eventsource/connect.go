package eventsource

import (
	"github.com/aws/aws-lambda-go/events"
)

// ConnectEvent is events.ConnectEvent (an Amazon Connect contact flow
// invocation) whose contact data has the media streams and tells absent
// endpoints and queues from empty ones.
type ConnectEvent struct {
	events.ConnectEvent

	Details ConnectDetails `json:"Details"`
}

// ConnectDetails is events.ConnectDetails with the complete contact data.
type ConnectDetails struct {
	events.ConnectDetails

	ContactData ConnectContactData `json:"ContactData"`
}

// ConnectContactData is events.ConnectContactData with the media streams.
// The customer endpoint, the queue and the system endpoint are nil when the
// contact has none, as Powertools Python returns None.
type ConnectContactData struct {
	events.ConnectContactData

	CustomerEndpoint *events.ConnectEndpoint `json:"CustomerEndpoint"`
	Queue            *events.ConnectQueue    `json:"Queue"`
	SystemEndpoint   *events.ConnectEndpoint `json:"SystemEndpoint"`
	MediaStreams     ConnectMediaStreams     `json:"MediaStreams"`
}

// ConnectMediaStreams describes the live media streams of the contact.
type ConnectMediaStreams struct {
	Customer ConnectCustomerMediaStream `json:"Customer"`
}

// ConnectCustomerMediaStream is the customer's media stream.
type ConnectCustomerMediaStream struct {
	Audio ConnectAudioStream `json:"Audio"`
}

// ConnectAudioStream is a Kinesis Video Streams audio stream. The values
// are absent until streaming starts.
type ConnectAudioStream struct {
	StartFragmentNumber string `json:"StartFragmentNumber,omitempty"`
	StartTimestamp      string `json:"StartTimestamp,omitempty"`
	StreamARN           string `json:"StreamARN,omitempty"`
}

// The channels of a contact.
const (
	ConnectChannelVoice = "VOICE"
	ConnectChannelChat  = "CHAT"
)

// ConnectEndpointTelephoneNumber is the type of a telephone endpoint.
const ConnectEndpointTelephoneNumber = "TELEPHONE_NUMBER"

// The ways a contact can be initiated.
const (
	ConnectInitiationInbound  = "INBOUND"
	ConnectInitiationOutbound = "OUTBOUND"
	ConnectInitiationTransfer = "TRANSFER"
	ConnectInitiationCallback = "CALLBACK"
	ConnectInitiationAPI      = "API"
)
