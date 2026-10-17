package sub2api

import (
	"context"
	"encoding/json"
	"time"
)

// Target is one source's local key bindings. Only the callbacks receive Key.Value.
type Target struct {
	ID, Name, Site, SessionID, GroupID, GroupName string
	BindingVersion                                string
	Generation                                    uint64
	Revision                                      string
	Context                                       string `json:"-"`
	Keys                                          []Key
	ActiveKeyIDs                                  []string
	RetainedKeyIDs                                []string
	WatchGroups                                   []Group
}

type Key struct {
	ID        string
	GroupID   string
	GroupName string
	Value     string `json:"-"`
	Context   string `json:"-"`
}

type Group struct {
	ID                 string                    `json:"id"`
	Name               string                    `json:"name"`
	Visible            *bool                     `json:"visible,omitempty"`
	Rate               *float64                  `json:"rate,omitempty"`
	Enabled            *bool                     `json:"enabled,omitempty"`
	Observation        *Observation              `json:"observation,omitempty"`
	LastSuccess        time.Time                 `json:"lastSuccess,omitempty"`
	ObservedAt         time.Time                 `json:"monitorLastSuccess,omitempty"`
	Error              string                    `json:"error,omitempty"`
	LastVisible        *bool                     `json:"lastVisible,omitempty"`
	LastVisibleAt      time.Time                 `json:"lastVisibleAt,omitempty"`
	LastRate           *float64                  `json:"lastRate,omitempty"`
	LastRateAt         time.Time                 `json:"lastRateAt,omitempty"`
	LastEnabled        *bool                     `json:"lastEnabled,omitempty"`
	LastEnabledAt      time.Time                 `json:"lastEnabledAt,omitempty"`
	LastObservation    *Observation              `json:"lastObservation,omitempty"`
	LastObservationAt  time.Time                 `json:"lastObservationAt,omitempty"`
	ObservationHistory map[string]ObservedMetric `json:"observationHistory,omitempty"`
}

// ObservedMetric keeps one metric's time and provenance, without mixing versions.
type ObservedMetric struct {
	Observation
	ObservedAt time.Time `json:"observedAt"`
}

type Observation struct {
	Source         string    `json:"source"`
	Scope          string    `json:"scope"`
	Window         string    `json:"window"`
	SampledAt      time.Time `json:"sampledAt,omitempty"`
	MatchedName    string    `json:"matchedName,omitempty"`
	Match          string    `json:"match"`
	Approximate    bool      `json:"approximate"`
	Availability   *float64  `json:"availability,omitempty"`
	ProbeLatencyMS *float64  `json:"probeLatencyMs,omitempty"`
	TTFTP50MS      *float64  `json:"ttftP50Ms,omitempty"`
	CacheRate      *float64  `json:"cacheRate,omitempty"`
}

type KeySnapshot struct {
	ID          string    `json:"id"`
	GroupID     string    `json:"groupId"`
	GroupName   string    `json:"groupName"`
	Rate        *float64  `json:"rate,omitempty"`
	LastRate    *float64  `json:"lastRate,omitempty"`
	LastRateAt  time.Time `json:"lastRateAt,omitempty"`
	LastSuccess time.Time `json:"lastSuccess,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type SourceSnapshot struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	GroupID        string        `json:"groupId"`
	GroupName      string        `json:"groupName"`
	Keys           []KeySnapshot `json:"keys"`
	Rate           *float64      `json:"rate,omitempty"`
	Conflict       bool          `json:"conflict"`
	BindingVersion string        `json:"bindingVersion"`
	Revision       string        `json:"revision"`
	ActiveKeyIDs   []string      `json:"activeKeyIds"`
}

func (s *SourceSnapshot) UnmarshalJSON(body []byte) error {
	type plain SourceSnapshot
	var value struct {
		*plain
		BindingVersion json.RawMessage `json:"bindingVersion"`
	}
	value.plain = (*plain)(s)
	if err := json.Unmarshal(body, &value); err != nil {
		return err
	}
	if len(value.BindingVersion) == 0 || string(value.BindingVersion) == "null" {
		return nil
	}
	if value.BindingVersion[0] == '"' {
		return json.Unmarshal(value.BindingVersion, &s.BindingVersion)
	}
	var legacy uint64
	if err := json.Unmarshal(value.BindingVersion, &legacy); err != nil {
		return err
	}
	// Numeric draft tickets are history, never authorization for a new write.
	s.BindingVersion, s.Revision, s.Rate, s.ActiveKeyIDs = "", "", nil, nil
	for i := range s.Keys {
		key := &s.Keys[i]
		if key.Rate != nil {
			key.LastRate, key.LastRateAt = key.Rate, key.LastSuccess
		}
		key.Rate = nil
	}
	return nil
}

type MonitorAccount struct {
	SessionID              string                    `json:"sessionId"`
	Generation             uint64                    `json:"generation"`
	SettingsRevision       string                    `json:"settingsRevision"`
	Configured             bool                      `json:"configured"`
	Site                   string                    `json:"site"`
	Enabled                bool                      `json:"enabled"`
	Watched                []Group                   `json:"watchedGroups"`
	Groups                 []Group                   `json:"groups"`
	GroupsKnown            bool                      `json:"groupsKnown"`
	Complete               bool                      `json:"groupsComplete"`
	Observation            *Observation              `json:"observation,omitempty"`
	LastObservation        *Observation              `json:"lastObservation,omitempty"`
	LastObservationAt      time.Time                 `json:"lastObservationAt,omitempty"`
	ObservationHistory     map[string]ObservedMetric `json:"observationHistory,omitempty"`
	Sources                []SourceSnapshot          `json:"sources"`
	LastAttempt            time.Time                 `json:"lastAttempt,omitempty"`
	LastSuccess            time.Time                 `json:"lastSuccess,omitempty"`
	NextPoll               time.Time                 `json:"nextPoll,omitempty"`
	Error                  string                    `json:"error,omitempty"`
	GroupsError            string                    `json:"groupsError,omitempty"`
	BillingError           string                    `json:"billingError,omitempty"`
	ObservationError       string                    `json:"observationError,omitempty"`
	GroupsLastSuccess      time.Time                 `json:"groupsLastSuccess,omitempty"`
	BillingLastSuccess     time.Time                 `json:"billingLastSuccess,omitempty"`
	ObservationLastSuccess time.Time                 `json:"observationLastSuccess,omitempty"`
	NeedsAction            bool                      `json:"needsAction,omitempty"`
	ActionResources        map[string]bool           `json:"actionResources,omitempty"`
	NotificationError      string                    `json:"notificationError,omitempty"`
}

type ChangeItem struct {
	Kind      string    `json:"kind"`
	GroupID   string    `json:"groupId,omitempty"`
	GroupName string    `json:"groupName,omitempty"`
	SourceID  string    `json:"sourceId,omitempty"`
	KeyID     string    `json:"keyId,omitempty"`
	Before    string    `json:"before,omitempty"`
	After     string    `json:"after,omitempty"`
	BeforeAt  time.Time `json:"beforeAt,omitempty"`
	AfterAt   time.Time `json:"afterAt,omitempty"`
}

// Change combines one account's changes in one round, so notifications do not fan out per key.
type Change struct {
	ID                 string                   `json:"id"`
	SessionID          string                   `json:"sessionId"`
	Site               string                   `json:"site"`
	At                 time.Time                `json:"at"`
	Items              []ChangeItem             `json:"items"`
	WithCurrent        func(func() error) error `json:"-"`
	reportNotification func(error)
}

func (c Change) ReportNotification(err error) {
	if c.reportNotification != nil {
		c.reportNotification(err)
	}
}

// MonitorSettings is the safe control state, not an instruction to start polling.
type MonitorSettings struct {
	Configured bool    `json:"configured"`
	Enabled    bool    `json:"enabled"`
	Groups     []Group `json:"groups"`
	Revision   string  `json:"revision"`
	Generation uint64  `json:"generation"`
}

type Snapshot struct {
	Accounts  []MonitorAccount `json:"accounts"`
	Changes   []Change         `json:"changes"`
	UpdatedAt time.Time        `json:"updatedAt,omitempty"`
	Running   bool             `json:"running"`
	Polling   bool             `json:"polling"`
}

type PanelRead func(context.Context, string, string) ([]byte, error)
type BillingRead func(context.Context, Target, Key) ([]byte, error)
