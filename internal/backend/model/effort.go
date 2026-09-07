package model

import "fmt"

// ValidateEffort bounds an explicit native option. Provider/model support and
// native policy may affect the applied value; this is never effective-state proof.
func ValidateEffort(value string) error {
	if len(value) > 0 && !(value[0] >= 'a' && value[0] <= 'z' || value[0] >= '0' && value[0] <= '9') {
		return fmt.Errorf("requested native effort must start with a letter or digit")
	}
	if len(value) > 64 {
		return fmt.Errorf("requested native effort exceeds 64 bytes")
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return fmt.Errorf("requested native effort must be a lowercase level or variant token")
		}
	}
	return nil
}
