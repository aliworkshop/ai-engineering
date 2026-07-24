package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	openai "github.com/sashabaranov/go-openai"
)

// GetWeather reports the current weather for a place by name. Read-only, so no
// approval is needed. It uses Open-Meteo's free, keyless APIs: first geocode the
// place to coordinates, then fetch the current conditions there.
type GetWeather struct {
	HTTP *http.Client
}

func (GetWeather) Spec() openai.Tool {
	return defineTool("get_weather",
		"Get the current weather (temperature and wind) for a city or place by name.",
		`{"type":"object","properties":{"location":{"type":"string","description":"City or place name, e.g. 'Paris' or 'Tokyo, Japan'"}},"required":["location"]}`)
}

func (t GetWeather) Run(ctx context.Context, args string) (string, error) {
	var a struct {
		Location string `json:"location"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.Location == "" {
		return "", fmt.Errorf("location is required")
	}

	lat, lon, name, err := t.geocode(ctx, a.Location)
	if err != nil {
		return "", err
	}

	endpoint := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f&current_weather=true",
		lat, lon)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out struct {
		CurrentWeather struct {
			Temperature float64 `json:"temperature"`
			Windspeed   float64 `json:"windspeed"`
		} `json:"current_weather"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}

	return fmt.Sprintf("Current weather in %s: %.1f°C, wind %.1f km/h.",
		name, out.CurrentWeather.Temperature, out.CurrentWeather.Windspeed), nil
}

// geocode turns a place name into coordinates using Open-Meteo's geocoder,
// returning the matched canonical name so the answer names the place it found.
func (t GetWeather) geocode(ctx context.Context, place string) (lat, lon float64, name string, err error) {
	endpoint := "https://geocoding-api.open-meteo.com/v1/search?count=1&name=" + url.QueryEscape(place)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return 0, 0, "", err
	}
	defer resp.Body.Close()

	var out struct {
		Results []struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Name      string  `json:"name"`
			Country   string  `json:"country"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, 0, "", err
	}
	if len(out.Results) == 0 {
		return 0, 0, "", fmt.Errorf("no place found named %q", place)
	}
	r := out.Results[0]
	name = r.Name
	if r.Country != "" {
		name += ", " + r.Country
	}
	return r.Latitude, r.Longitude, name, nil
}
