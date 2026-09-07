package model

const MaxGroupCapacity = 2147483647

func ValidGroupCapacity(value int64) bool { return value >= 0 && value <= MaxGroupCapacity }
