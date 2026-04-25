package stats

import "time"

type AlgoStat struct {
    AlgoID         int
    AlgoName       string
    Difficulty     float64
    Hashrate       float64
    PeakHashrate   float64
    MoneySupply    int64
    BlockReward    float64
    NSSFRemaining  int
    BlockSpacing   float64
    ComputedAt	   time.Time
}

type StatsPageData struct {
    GeneratedAt time.Time
    Algos       []AlgoStat
}
