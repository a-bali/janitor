package main

import "time"

// counterWindowDays bounds sample and total counters to a rolling window so
// long-term persistence does not accumulate all-time counts.
const counterWindowDays = 7

const dayLayout = "2006-01-02"

// dayBuckets counts events per UTC day, enabling bounded rolling-window totals.
type dayBuckets map[string]int

// recordBucket adds an event to the current day's bucket, allocating the map if needed.
func recordBucket(buckets dayBuckets, now time.Time) dayBuckets {
	if buckets == nil {
		buckets = make(dayBuckets)
	}
	buckets[now.UTC().Format(dayLayout)]++
	return buckets
}

// total prunes buckets outside the window and returns the remaining count.
func (buckets dayBuckets) total(now time.Time) int64 {
	cutoff := now.UTC().AddDate(0, 0, -(counterWindowDays - 1)).Format(dayLayout)
	var sum int64
	for day, count := range buckets {
		if day < cutoff {
			delete(buckets, day)
			continue
		}
		sum += int64(count)
	}
	return sum
}

func copyDayBuckets(source dayBuckets) dayBuckets {
	if source == nil {
		return nil
	}
	result := make(dayBuckets, len(source))
	for day, count := range source {
		result[day] = count
	}
	return result
}
