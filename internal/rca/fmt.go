package rca

import "strconv"

func strconvFormat(v float64, fmt byte, prec int) string {
	return strconv.FormatFloat(v, fmt, prec, 64)
}
