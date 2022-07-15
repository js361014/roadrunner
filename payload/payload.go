package payload

import (
	"github.com/js361014/roadrunner/v2/utils"
)

// Payload carries binary header and body to stack and
// back to the server.
type Payload struct {
	// Context represent payload context, might be omitted.
	Context []byte

	// body contains binary payload to be processed by WorkerProcess.
	Body []byte

	// Type of codec used to decode/encode payload
	Codec byte
}

// String returns payload body as string
func (p *Payload) String() string {
	return utils.AsString(p.Body)
}
