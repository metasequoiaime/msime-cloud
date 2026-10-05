// Code generated from contracts/protocol.json. DO NOT EDIT.
package contract

const (
	APIVersion                 = "1"
	HealthPath                 = "/healthz"
	CapabilitiesPath           = "/v1/capabilities"
	CloudPath                  = "/v1/cloud/candidates"
	ChatPath                   = "/v1/chat/completions"
	TranslationPath            = "/v1/translate"
	TranscriptionPath          = "/v1/audio/transcriptions"
	ModelsPath                 = "/v1/models"
	StreamingTranscriptionPath = "/v1/audio/stream"
	JsonBodyBytes              = 65536
	UpstreamResponseBytes      = 1048576
	AudioFileBytes             = 15728640
	MultipartBodyBytes         = 16777216
	CloudInputBytes            = 256
	CandidateBytes             = 512
	CloudCandidates            = 10
	ChatMessages               = 16
	ChatMessageBytes           = 16384
	ChatMaxTokens              = 2048
	TranslationInputBytes      = 8192
	OutputTextBytes            = 65536
	ChatDefaultTokens          = 2048
	StreamMessageBytes         = 1048576
	StreamSessionBytes         = 33554432
)
