package render

import (
	"encoding/json"
	"io"

	"github.com/JaydenCJ/buildbust/internal/cachekey"
	"github.com/JaydenCJ/buildbust/internal/diff"
)

// jsonSchemaVersion versions the CLI's JSON envelopes (independent of the
// snapshot file's schema_version).
const jsonSchemaVersion = 1

type envelope struct {
	Tool          string `json:"tool"`
	SchemaVersion int    `json:"schema_version"`
	Meta          Meta   `json:"meta"`
}

type explainEnvelope struct {
	envelope
	*diff.Result
}

// ExplainJSON renders the comparison result as stable JSON.
func ExplainJSON(w io.Writer, res *diff.Result, meta Meta) error {
	return writeJSON(w, explainEnvelope{
		envelope: envelope{Tool: "buildbust", SchemaVersion: jsonSchemaVersion, Meta: meta},
		Result:   res,
	})
}

type filesEnvelope struct {
	envelope
	Stages []string        `json:"stages"`
	Steps  []cachekey.Step `json:"steps"`
}

// FilesJSON renders the COPY/ADD file inventory as stable JSON.
func FilesJSON(w io.Writer, plan *cachekey.Plan, meta Meta) error {
	steps := make([]cachekey.Step, 0)
	for _, s := range plan.Steps {
		if s.Cmd == "COPY" || s.Cmd == "ADD" {
			steps = append(steps, s)
		}
	}
	return writeJSON(w, filesEnvelope{
		envelope: envelope{Tool: "buildbust", SchemaVersion: jsonSchemaVersion, Meta: meta},
		Stages:   plan.Stages,
		Steps:    steps,
	})
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
