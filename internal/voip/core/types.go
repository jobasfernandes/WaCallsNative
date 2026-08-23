package core

type CallState string

const (
	CallStateInitiating      CallState = "initiating"
	CallStateRinging         CallState = "ringing"
	CallStateIncomingRinging CallState = "incoming_ringing"
	CallStateConnecting      CallState = "connecting"
	CallStateActive          CallState = "active"
	CallStateOnHold          CallState = "on_hold"
	CallStateReconnecting    CallState = "reconnecting"
	CallStateEnded           CallState = "ended"
)

type CallDirection string

const (
	CallDirectionOutgoing CallDirection = "outgoing"
	CallDirectionIncoming CallDirection = "incoming"
)

type CallMediaType string

const (
	CallMediaTypeAudio CallMediaType = "audio"
)

type EndCallReason string

const (
	EndCallReasonUserEnded    EndCallReason = "user_ended"
	EndCallReasonDeclined     EndCallReason = "declined"
	EndCallReasonTimeout      EndCallReason = "timeout"
	EndCallReasonBusy         EndCallReason = "busy"
	EndCallReasonCancelled    EndCallReason = "cancelled"
	EndCallReasonFailed       EndCallReason = "failed"
	EndCallReasonDoNotDisturb EndCallReason = "do_not_disturb"
	EndCallReasonUnknown      EndCallReason = "unknown"
)

const (
	PayloadTypeWhatsAppOpus = 120
	// WhatsApp sends the same audio stream under either payload type; group
	// calls use 121 throughout, and a receiver bound to 120 alone hears nobody.
	PayloadTypeWhatsAppOpusAlt = 121
	PayloadTypeWhatsAppAppData = 119
)

// SSRC counters index the ~12-SSRC set WhatsApp allocates per device; both sides
// derive the same values from (callID, deviceJID, counter), which is how they
// learn each other's SSRCs without SDP. Values come from captured traffic.
const (
	SsrcCounterAudio   uint32 = 0
	SsrcCounterAppData uint32 = 6
)

const (
	SRTPSendAuthTagLen = 4
	SRTPRecvAuthTagLen = 4
	SRTPAuthTagLen     = 4
)

const (
	SRTPLabelEncryption = 0x00
	SRTPLabelAuth       = 0x01
	SRTPLabelSalt       = 0x02
)

const WARelayPort = 3480

const WADTLSFingerprint = "sha-256 F9:CA:0C:98:A3:CC:71:D6:42:CE:5A:E2:53:D2:15:20:D3:1B:BA:D8:57:A4:F0:AF:BE:0B:FB:F3:6B:0C:A0:68"

type SrtpKeyingMaterial struct {
	MasterKey  []byte
	MasterSalt []byte
}

type RelayEndpoint struct {
	IP           string
	Port         int
	Token        string
	AuthToken    string
	RawAuthToken []byte
	RawToken     []byte
	Key          string
	RelayID      int
	Protocol     int
	C2RRtt       *int
	RelayName    string
	AddressBytes []byte
	AuthTokenID  string
	IsFNA        bool
}

type RelayData struct {
	Endpoints       []RelayEndpoint
	ParticipantJids []string
	UUID            string
	SelfPid         *int
	PeerPid         *int
	HbhKey          []byte
}

type AudioEngineConfig struct {
	SampleRate         int
	CaptureChunkSize   int
	PlaybackOutputSize int
	MaxBufferSize      int
	IntervalMs         int
}

var DefaultAudioConfig = AudioEngineConfig{
	SampleRate:         16000,
	CaptureChunkSize:   320,
	PlaybackOutputSize: 256,
	MaxBufferSize:      1600,
	IntervalMs:         20,
}

// IsWhatsAppAudioPayload reports whether a payload type carries WhatsApp audio.
func IsWhatsAppAudioPayload(pt uint8) bool {
	return pt == PayloadTypeWhatsAppOpus || pt == PayloadTypeWhatsAppOpusAlt
}
