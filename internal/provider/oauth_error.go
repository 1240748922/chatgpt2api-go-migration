package provider

import "fmt"

type OAuthRefreshError struct {
	Status    int
	Uncertain bool
	Message   string
}

func (e *OAuthRefreshError) Error() string {
	return fmt.Sprintf("oauth refresh HTTP %d: %s", e.Status, e.Message)
}
