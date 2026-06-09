// internal/setup/answers_test.go
package setup

import "testing"

func validAnswers() Answers {
	return Answers{
		Hostname:      "img.example.com",
		ContactEmail:  "abuse@example.com",
		AdminEmail:    "op@example.com",
		AdminPassword: "correct horse battery", // >= 12 chars
		TLSMode:       "dev-self-signed",
		AuthMode:      "local",
	}
}

func TestAnswersValidate_OK(t *testing.T) {
	if err := validAnswers().Validate(); err != nil {
		t.Fatalf("valid answers rejected: %v", err)
	}
}

func TestAnswersValidate_Rejections(t *testing.T) {
	cases := map[string]func(*Answers){
		"missing hostname":      func(a *Answers) { a.Hostname = "" },
		"missing contact":       func(a *Answers) { a.ContactEmail = "" },
		"short password":        func(a *Answers) { a.AdminPassword = "short" },
		"bad tls mode":          func(a *Answers) { a.TLSMode = "bogus" },
		"static without paths":  func(a *Answers) { a.TLSMode = "static" },
		"public uploads no tos": func(a *Answers) { a.PublicUploads = true; a.TosURL = "" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			a := validAnswers()
			mut(&a)
			if err := a.Validate(); err == nil {
				t.Fatalf("%s: expected validation error", name)
			}
		})
	}
}
