package internal

const (
	// This is the port the init container will listen on
	// and wait for the nebula-cni-instance to call it on
	InitPort = "516"

	HeaderVersion = "X-Version"
)

type InitPostBody struct {
	NebulaCertificate string `json:"nebula_certificate"`
}
