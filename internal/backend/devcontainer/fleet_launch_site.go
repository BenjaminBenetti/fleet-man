package devcontainer

// FleetLaunchSite is a single entry on the Fleet Launch page.
type FleetLaunchSite struct {
	// Title is the primary label shown for the link.
	Title string `json:"title"`
	// Icon is an optional image shown before the title. Its value is used
	// verbatim as an <img> src, so any path the browser can load works
	// (e.g. an https URL or a data URI). An empty value means "no icon".
	Icon string `json:"icon"`
	// SubTitle is the secondary descriptive text shown under the title.
	SubTitle string `json:"subTitle"`
	// URL is the address the link navigates to.
	URL string `json:"url"`
	// HealthCheck is an address polled to indicate whether the service
	// is reachable. An empty value means "no health check".
	HealthCheck string `json:"healthCheck"`
}
