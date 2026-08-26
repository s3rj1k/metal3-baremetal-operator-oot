// SPDX-License-Identifier: Apache-2.0

// The kickstart route. Anaconda fetches it during boot, identifying itself only
// by the MACs it reports, so everything is resolved from the request.

package httpapi

import (
	"bytes"
	"cmp"
	"io"
	"net/http"
	"slices"
	"strings"
	"text/template"

	"metal3.local/anaconda/internal/core"
)

// KickstartPathPrefix is the fixed route prefix anaconda fetches a kickstart from.
const KickstartPathPrefix = "/ks/"

// RenderTemplate executes content with vars as the root context. A missing key
// is an error so template typos surface instead of emitting nothing.
func RenderTemplate(name, content string, vars any) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Parse(content)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", err
	}

	return buf.String(), nil
}

// KickstartVars is the template context. Every field is always set because
// RenderTemplate runs with missingkey=error and should fail on a typo.
type KickstartVars struct {
	Name        string
	Namespace   string
	UID         string
	BootMAC     string
	CallbackURL string
	// InstallDisk is the disk the storage commands wipe, so an empty one has to
	// stop the render rather than reach a live machine.
	InstallDisk string
}

// InstallDiskVar is the template field guarded before a kickstart is served. An
// unresolved disk renders ignoredisk with no value, which anaconda cannot parse.
const InstallDiskVar = ".InstallDisk"

// KickstartSections are the directives the parser recognizes at column 0. Script
// bodies have to keep other % lines indented or the section ends early.
var KickstartSections = []string{
	"%addon", "%anaconda", "%end", "%include", "%ksappend", "%onerror",
	"%packages", "%post", "%pre", "%pre-install", "%traceback",
}

// IsKickstartSection reports whether a line opens a directive the parser knows.
func IsKickstartSection(line string) bool {
	name, _, _ := strings.Cut(strings.TrimSpace(line), " ")

	return slices.Contains(KickstartSections, name)
}

// WriteKickstart emits a kickstart body.
func WriteKickstart(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	//nolint:gosec // a kickstart is served as text and never rendered as markup
	_, _ = io.WriteString(w, content)
}

// ServeFallbackKickstart emits the compiled in no-op kickstart. It answers 200
// on purpose, a 404 drops anaconda to an interactive prompt on a live machine.
func (s *PluginServer) ServeFallbackKickstart(w http.ResponseWriter, reason string, macs []string) {
	s.Log.Info("kickstart: serving fallback", "reason", reason, "macs", macs)

	WriteKickstart(w, DefaultFallbackKickstart)
}

// KickstartVarsFor builds the render context, including the callback endpoint
// the install reports itself finished on.
func (s *PluginServer) KickstartVarsFor(host core.HostRef) KickstartVars {
	vars := KickstartVars{
		Name:      host.Name,
		Namespace: host.Namespace,
		UID:       host.UID,
		BootMAC:   host.BootMAC,
		// The host's own hint wins, so one mismatched machine in a fleet is fixed
		// on its BareMetalHost instead of in the operator's config.
		InstallDisk: cmp.Or(host.InstallDisk, s.Config.InstallDisk),
	}

	if s.Config.BaseURL != "" {
		vars.CallbackURL = CallbackURL(s.Config.BaseURL, host.UID, host.Namespace, host.Name)
	}

	return vars
}

// HandleKickstart resolves the caller from the MACs anaconda reported and serves
// that host's kickstart. Anything unresolved gets the fallback, never a wipe.
func (s *PluginServer) HandleKickstart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	macs := AnacondaMACs(r)

	if len(macs) == 0 {
		// Without inst.ks.sendmac on the kernel cmdline there is nothing to match on.
		s.ServeFallbackKickstart(w, "request carried no MAC headers", macs)

		return
	}

	hosts, err := s.Resolver.FindHostsByMAC(r.Context(), macs)
	if err != nil {
		s.Log.Error(err, "kickstart: host lookup failed", "macs", macs)
		http.Error(w, "host lookup failed", http.StatusInternalServerError)

		return
	}

	if len(hosts) == 0 {
		s.ServeFallbackKickstart(w, "no host declares these MACs as its boot MAC", macs)

		return
	}

	if len(hosts) > 1 {
		s.Log.Error(nil, "kickstart: multiple hosts claim these MACs, using the first",
			"macs", macs, "hosts", hosts)
	}

	host := hosts[0]

	// The kickstart lives in the host's own preprovisioning Secret, read at
	// request time, so a host is servable the moment it exists.
	content, found, err := s.Resolver.ReadKickstart(r.Context(), host.Namespace, host.KickstartSecret)
	if err != nil {
		s.Log.Error(err, "kickstart: secret read failed", "host", host.Name, "secret", host.KickstartSecret)
		http.Error(w, "kickstart read failed", http.StatusInternalServerError)

		return
	}

	if !found {
		s.ServeFallbackKickstart(w,
			"host names no preprovisioning Secret carrying a "+core.KickstartSecretKey+" key", macs)

		return
	}

	vars := s.KickstartVarsFor(host)

	// Only templates that actually name the var are held to it, so a kickstart
	// carrying its own hardcoded disk keeps working with no default configured.
	if vars.InstallDisk == "" && strings.Contains(content, InstallDiskVar) {
		s.ServeFallbackKickstart(w,
			"template needs "+InstallDiskVar+" but the host hints no disk and "+core.EnvInstallDisk+" is unset", macs)

		return
	}

	rendered, err := RenderTemplate(id, content, vars)
	if err != nil {
		s.Log.Error(err, "kickstart: template render failed", "host", host.Name, "id", id)
		http.Error(w, "template render failed", http.StatusInternalServerError)

		return
	}

	s.Log.Info("kickstart: served", "host", host.Name, "id", id, "secret", host.KickstartSecret)
	WriteKickstart(w, rendered)
}
