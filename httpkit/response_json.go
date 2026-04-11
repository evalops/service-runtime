package httpkit

import (
	"encoding/json"
	"io"
)

func newJSONEncoder(writer io.Writer) jsonEncoder {
	return json.NewEncoder(writer)
}
