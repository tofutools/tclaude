package model

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

type Diagnostic struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Path     string   `json:"path,omitempty"`
	Message  string   `json:"message"`
	// Line and Col are 1-based source positions carried from the YAML node that
	// produced the diagnostic. They are only populated for diagnostics raised
	// during the raw-node walk (unknown fields, duplicate keys, merge keys);
	// semantic diagnostics computed from the decoded template leave them zero.
	Line int `json:"line,omitempty"`
	Col  int `json:"col,omitempty"`
}

type Diagnostics []Diagnostic

func (d Diagnostics) HasErrors() bool {
	for _, diag := range d {
		if diag.Severity == SeverityError {
			return true
		}
	}
	return false
}

func (d Diagnostics) Errors() Diagnostics {
	return d.withSeverity(SeverityError)
}

func (d Diagnostics) Warnings() Diagnostics {
	return d.withSeverity(SeverityWarning)
}

func (d Diagnostics) withSeverity(severity Severity) Diagnostics {
	var out Diagnostics
	for _, diag := range d {
		if diag.Severity == severity {
			out = append(out, diag)
		}
	}
	return out
}

func diagError(code, path, message string) Diagnostic {
	return Diagnostic{Severity: SeverityError, Code: code, Path: path, Message: message}
}

func diagWarning(code, path, message string) Diagnostic {
	return Diagnostic{Severity: SeverityWarning, Code: code, Path: path, Message: message}
}
