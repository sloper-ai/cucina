// SPDX-License-Identifier: FSL-1.1-ALv2

package grpcuploads

import (
	"fmt"
	"sort"
)

// Evaluate joins every offered Write payload with a completed cold repository
// digest baseline and explicit release inventory. The same digest may have
// several owners; totals and component credits assign it once, deterministically.
// Ordinary-action-context writes are included, not only repo-labelled RPCs.
func Evaluate(inventory Inventory, captures []Capture) Report {
	var out Report
	missing, bad := map[string]bool{}, map[string]bool{}
	if err := inventory.Validate(); err != nil {
		return Report{Unavailable: []string{err.Error()}}
	}
	repos := map[string]Repository{}
	components := map[string]*Component{}
	exercised := map[string]map[string]bool{}
	for _, r := range inventory.Repositories {
		repos[r.Name] = r
		components[r.Name] = &Component{Repository: r}
		exercised[r.Name] = map[string]bool{}
	}
	owners := map[string]map[string]bool{}
	digests := map[string]Digest{}
	housekeeping := map[string]bool{}
	complete := map[string]bool{}
	assessed := map[string]bool{}
	configSeen := map[string]bool{}
	scope, hashFunction := "", ""
	for _, c := range captures {
		t := c.Trace
		if !hashRE.MatchString(t.ClientProvenance) {
			missing["actual-client repository provenance not recorded"] = true
		}
		if t.InventoryDigest != inventory.Fingerprint() {
			missing["evidence repository/version inventory differs"] = true
		}
		if t.SchemaVersion != 1 || t.Config == "" || t.InvocationID == "" || t.Entries == 0 {
			missing["missing or empty invocation RPC evidence"] = true
			continue
		}
		if scope == "" {
			scope = t.Scope
			hashFunction = t.HashFunction
		}
		if t.Scope != scope || t.HashFunction != hashFunction {
			missing["configuration CAS scope/digest function differs"] = true
		}
		configSeen[t.Config] = true
		for _, issue := range t.Issues {
			missing[issue] = true
		}
		for _, m := range t.Manifests {
			assessed[m.Repository] = true
			if _, ok := repos[m.Repository]; !ok {
				missing["manifest has no explicit repository/version pin"] = true
				continue
			}
			if !m.Complete || len(m.Digests) == 0 {
				missing[m.Repository+": incomplete repository baseline"] = true
				continue
			}
			complete[m.Repository] = true
			exercised[m.Repository][t.Config] = true
			for _, d := range m.Digests {
				key := d.Key()
				if owners[key] == nil {
					owners[key] = map[string]bool{}
				}
				owners[key][m.Repository] = true
				digests[key] = d
			}
			for _, d := range m.Housekeeping {
				housekeeping[d.Key()] = true
			}
		}
	}
	if len(configSeen) < 2 {
		missing["at least two distinct observed configurations are required"] = true
	}

	// Keep memberships many-to-many even when one upload is credited once.
	for _, c := range captures {
		for _, f := range c.Files {
			assessed[f.Repository] = true
			if _, ok := repos[f.Repository]; !ok {
				missing["used file has no explicit repository pin"] = true
				continue
			}
			exercised[f.Repository][c.Trace.Config] = true
			if !complete[f.Repository] || !owners[f.Digest.Key()][f.Repository] {
				missing[f.Repository+": used file missing from complete baseline"] = true
			}
		}
	}
	primary := map[string]string{}
	for key, rs := range owners {
		names := make([]string, 0, len(rs))
		for name := range rs {
			names = append(names, name)
		}
		sort.Strings(names)
		primary[key] = names[0]
	}
	// Only observed successful uploads count as initially uploaded logical
	// content; repeated attempts still contribute their real offered bytes.
	credited := map[string]bool{}
	completedDigests := map[string]bool{}
	firstConfig := map[string]string{}
	attempts := map[string]int{}
	attemptWrites := map[string]Write{}
	for _, c := range captures {
		for _, w := range c.Trace.Writes {
			key := w.Digest.Key()
			owner := primary[key]
			if owner == "" {
				if w.Repository != "" {
					assessed[w.Repository] = true
					missing[w.Repository+": uploaded digest absent from baseline"] = true
				}
				continue
			}
			if w.OfferedBytes < 0 {
				missing["negative offered bytes"] = true
				continue
			}
			out.OfferedBytes += w.OfferedBytes
			comp := components[owner]
			if housekeeping[key] {
				comp.HousekeepingBytes += w.OfferedBytes
			} else {
				comp.OfferedBytes += w.OfferedBytes
			}
			if w.OfferedBytes > 0 && !housekeeping[key] {
				if first, ok := firstConfig[key]; ok && first != c.Trace.Config {
					out.RepeatedConfigurationPayload += w.OfferedBytes
					bad["known repository content offered again by another configuration"] = true
				} else if !ok {
					firstConfig[key] = c.Trace.Config
				}
			}
			attempt := c.Trace.Config + "/" + w.Upload + "/" + key
			if attempts[attempt] > 0 {
				out.RetryAttempts++
			}
			attempts[attempt]++
			attemptWrites[attempt] = w
			completed := writeComplete(w)
			if w.Status == 0 && !completed {
				missing["successful RPC lacks valid completion/committed-size evidence"] = true
			}
			if completed {
				completedDigests[key] = true
			}
			if completed && w.OfferedBytes > 0 && !credited[key] && !housekeeping[key] {
				credited[key] = true
				out.UniqueContentBytes += w.Digest.Size
				comp.UniqueContentBytes += w.Digest.Size
			}
		}
	}
	for _, c := range captures {
		for _, q := range c.Trace.Queries {
			key := q.Digest.Key()
			if q.Status != 0 || !q.Complete || attempts[c.Trace.Config+"/"+q.Upload+"/"+key] == 0 {
				continue
			}
			w := attemptWrites[c.Trace.Config+"/"+q.Upload+"/"+key]
			if w.Compressor != q.Compressor {
				missing["resume confirmation compressor mismatch"] = true
				continue
			}
			w.Status = 0
			w.CommittedSize = &q.CommittedSize
			if !writeComplete(w) {
				missing["resume confirmation has invalid committed-size evidence"] = true
				continue
			}
			completedDigests[key] = true
			if !credited[key] && !housekeeping[key] && firstConfig[key] != "" {
				if owner := primary[key]; owner != "" {
					credited[key] = true
					out.UniqueContentBytes += q.Digest.Size
					components[owner].UniqueContentBytes += q.Digest.Size
				}
			}
		}
	}
	for key := range firstConfig {
		if !completedDigests[key] {
			missing["offered repository content has no completed upload or already-present confirmation"] = true
		}
	}

	names := make([]string, 0, len(repos))
	for name := range repos {
		names = append(names, name)
	}
	sort.Strings(names)
	initialEvidence := map[string]bool{}
	for key := range credited {
		for owner := range owners[key] {
			initialEvidence[owner] = true
		}
	}
	for _, name := range names {
		if !assessed[name] {
			continue
		}
		if !initialEvidence[name] {
			missing[name+": no observed initial content upload for this variant"] = true
		}
		if !complete[name] {
			missing[name+": no complete FindMissingBlobs repository baseline"] = true
		}
		for config := range exercised[name] {
			components[name].Configurations = append(components[name].Configurations, config)
		}
		sort.Strings(components[name].Configurations)
		if len(components[name].Configurations) < 2 {
			missing[name+": version was not exercised by two configurations"] = true
		}
		out.Components = append(out.Components, *components[name])
	}

	if out.UniqueContentBytes == 0 {
		missing["no initial repository content upload observed; cache reuse alone is not complete upload evidence"] = true
	}
	for m := range missing {
		out.Unavailable = append(out.Unavailable, m)
	}
	sort.Strings(out.Unavailable)
	for b := range bad {
		out.Violations = append(out.Violations, fmt.Sprintf("%s (%d offered bytes)", b, out.RepeatedConfigurationPayload))
	}
	sort.Strings(out.Violations)
	out.Pass = len(out.Unavailable) == 0 && len(out.Violations) == 0
	return out
}

func writeComplete(w Write) bool {
	if w.Status == 6 {
		return true
	} // server already has the blob; offered bytes still count
	if w.Status != 0 || w.CommittedSize == nil {
		return false
	}
	if w.Compressor == "zstd" && *w.CommittedSize == -1 {
		return true
	}
	if len(w.FinishWrites) > 0 {
		return *w.CommittedSize == w.FinishWrites[len(w.FinishWrites)-1]
	}
	// Early OK means the server already had the blob. For identity its
	// committed size is checkable; compressed early success uses -1 above.
	return w.Compressor == "identity" && *w.CommittedSize == w.Digest.Size
}
