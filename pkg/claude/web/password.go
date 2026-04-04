package web

import "go.1password.io/spg"

// generatePassword creates a random 16-character alphanumeric password.
func generatePassword() (string, error) {
	r := spg.NewCharRecipe(16)
	r.Allow = spg.Letters | spg.Digits
	r.Require = spg.Letters | spg.Digits
	pwd, err := r.Generate()
	if err != nil {
		return "", err
	}
	return pwd.String(), nil
}
