package marketingos

import "fmt"

// BoostOTPThresholdKey in Tenant.metadata is the boost total, in KES, from which launching a
// boost needs a WhatsApp confirmation code (audit M1). Absent means DefaultBoostOTPThresholdKES;
// 0 turns the code off.
const (
	BoostOTPThresholdKey        = "boost_otp_threshold_kes"
	DefaultBoostOTPThresholdKES = int64(5000) // owner decision 2026-11-17
	maxBoostOTPThresholdKES     = int64(100_000_000)
)

// BoostOTPThresholdKES reads the threshold from tenant metadata.
func BoostOTPThresholdKES(meta map[string]any) int64 {
	switch v := meta[BoostOTPThresholdKey].(type) {
	case float64:
		if v >= 0 {
			return int64(v)
		}
	case int64:
		if v >= 0 {
			return v
		}
	case int:
		if v >= 0 {
			return int64(v)
		}
	}
	return DefaultBoostOTPThresholdKES
}

// cleanBoostOTPThreshold checks the value an admin saves; nil keeps the default.
func cleanBoostOTPThreshold(v *int64) error {
	if v == nil {
		return nil
	}
	if *v < 0 || *v > maxBoostOTPThresholdKES {
		return fmt.Errorf("the boost confirmation threshold must be between 0 and %d KES", maxBoostOTPThresholdKES)
	}
	return nil
}

func readBoostOTPThreshold(meta map[string]any, s *Settings) {
	if _, set := meta[BoostOTPThresholdKey]; !set {
		return
	}
	v := BoostOTPThresholdKES(meta)
	s.BoostOTPThresholdKES = &v
}

func boostOTPThresholdValue(s Settings) any {
	if s.BoostOTPThresholdKES == nil {
		return nil
	}
	return *s.BoostOTPThresholdKES
}
