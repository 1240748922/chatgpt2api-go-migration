package httpapi

import (
	"encoding/base64"
	"fmt"
	"time"
)

func migrationTestJWT(subject string) string {
	claims := fmt.Sprintf(`{"sub":%q,"exp":%d}`, subject, time.Now().Add(time.Hour).Unix())
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".test"
}
