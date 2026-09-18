package apierr

import (
	"time"

	dpb "google.golang.org/protobuf/types/known/durationpb"
)

func durationpb(ms int64) *dpb.Duration {
	return dpb.New(time.Duration(ms) * time.Millisecond)
}
