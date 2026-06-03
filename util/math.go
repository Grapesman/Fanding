package util

import "math"

func RoundToTick(price, tick float64) float64 {
	if tick <= 0 {
		return price
	}
	return math.Round(price/tick) * tick
}

func SafeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}
