//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// PointerResultsFile — saved pointer scan results
type PointerResultsFile struct {
	Version    string         `json:"version"`
	SavedAt    time.Time      `json:"saved_at"`
	GameExe    string         `json:"game_exe"`
	Is32Bit    bool           `json:"is_32bit"`
	DataType   string         `json:"data_type"`
	Chains     []SavedChain   `json:"chains"`
}

type SavedChain struct {
	BaseModule    string   `json:"base_module"`
	BaseOffset    string   `json:"base_offset"`
	Offsets       []string `json:"offsets"`
	Label         string   `json:"label"`
	Notes         string   `json:"notes"`
	ExpectedValue string   `json:"expected_value"` // value at time of prsave
}

// formatHexOffset formats a uintptr offset as a signed hex literal: "0x1A0" or "-0x1A0".
// No quotes — meant to be embedded as a raw JSON token (non-strict JSON).
func formatHexOffset(o uintptr) string {
	signed := int64(o)
	if signed < 0 {
		return fmt.Sprintf("-0x%X", uint64(-signed))
	}
	return fmt.Sprintf("0x%X", o)
}

// parseHexOffset parses a hex string written by formatHexOffset OR any legacy
// format: "1A0", "+1A0", "-1A0", "0x1A0", "-0x1A0", with or without surrounding spaces.
func parseHexOffset(s string) uintptr {
	s = strings.TrimSpace(s)
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	s = strings.TrimPrefix(s, "+")
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	var v uint64
	fmt.Sscanf(s, "%X", &v)
	if negative {
		return uintptr(^v + 1) // two's complement
	}
	return uintptr(v)
}

func savedToChain(s SavedChain) (PointerChain, error) {
	offsets := make([]uintptr, len(s.Offsets))
	for i, o := range s.Offsets {
		offsets[i] = parseHexOffset(o)
	}
	return PointerChain{
		BaseModule: s.BaseModule,
		BaseOffset: parseHexOffset(s.BaseOffset),
		Offsets:    offsets,
	}, nil
}

// quoteHexLiterals wraps unquoted hex literals (`0x1A0`, `-0x1A0`) in double quotes
// so encoding/json can parse the file. State-machine over the bytes — does not
// touch text inside existing string literals.
func quoteHexLiterals(data []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(data) + 32)
	inString := false
	escape := false
	isHex := func(c byte) bool {
		return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
	}
	i := 0
	for i < len(data) {
		c := data[i]
		if inString {
			out.WriteByte(c)
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				inString = false
			}
			i++
			continue
		}
		if c == '"' {
			inString = true
			out.WriteByte(c)
			i++
			continue
		}
		// Look for: optional '-', then '0x', then hex digits.
		start := i
		end := i
		if c == '-' && end+2 < len(data) && data[end+1] == '0' && (data[end+2] == 'x' || data[end+2] == 'X') {
			end += 3
		} else if c == '0' && end+1 < len(data) && (data[end+1] == 'x' || data[end+1] == 'X') {
			end += 2
		} else {
			out.WriteByte(c)
			i++
			continue
		}
		for end < len(data) && isHex(data[end]) {
			end++
		}
		if end-start <= 2 || (data[start] == '-' && end-start <= 3) {
			out.WriteByte(c)
			i++
			continue
		}
		out.WriteByte('"')
		out.Write(data[start:end])
		out.WriteByte('"')
		i = end
	}
	return out.Bytes()
}

// SavePointerResults writes strict JSON with hex-prefixed strings for addresses:
//   "base_offset": "0xA945820",
//   "offsets":     ["0xD8", "0xB0", "0x0", "-0x1A0"]
// Strip the quotes when pasting into a Python tuple — `0x...` parses natively.
// We hand-roll the writer to keep offsets on one line (easier to copy).
func SavePointerResults(path string, results []PointerResult, gameExe string, is32Bit bool, dt DataType, handle windows.Handle, modules []ModuleInfo) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	jsonStr := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}

	fmt.Fprintln(f, "{")
	fmt.Fprintf(f, "  \"version\": %s,\n", jsonStr(AppVersion))
	fmt.Fprintf(f, "  \"saved_at\": %s,\n", jsonStr(time.Now().Format(time.RFC3339)))
	fmt.Fprintf(f, "  \"game_exe\": %s,\n", jsonStr(gameExe))
	fmt.Fprintf(f, "  \"is_32bit\": %t,\n", is32Bit)
	fmt.Fprintf(f, "  \"data_type\": %s,\n", jsonStr(dataTypeName(dt)))
	fmt.Fprintln(f, "  \"chains\": [")

	for i, r := range results {
		expectedValue := ""
		if handle != 0 {
			addr, ok := VerifyChain(handle, modules, r.Chain, is32Bit)
			if ok {
				val, err := ReadMemory(handle, addr, dataTypeSize(dt))
				if err == nil {
					expectedValue = decodeValue(dt, val)
				}
			}
		}

		fmt.Fprintln(f, "    {")
		fmt.Fprintf(f, "      \"base_module\": %s,\n", jsonStr(r.Chain.BaseModule))
		fmt.Fprintf(f, "      \"base_offset\": \"0x%X\",\n", r.Chain.BaseOffset)
		fmt.Fprint(f, "      \"offsets\": [")
		for j, o := range r.Chain.Offsets {
			if j > 0 {
				fmt.Fprint(f, ", ")
			}
			fmt.Fprintf(f, "%q", formatHexOffset(o))
		}
		fmt.Fprintln(f, "],")
		fmt.Fprintf(f, "      \"label\": %s,\n", jsonStr(r.Label))
		fmt.Fprintf(f, "      \"notes\": %s,\n", jsonStr(""))
		fmt.Fprintf(f, "      \"expected_value\": %s\n", jsonStr(expectedValue))

		if i < len(results)-1 {
			fmt.Fprintln(f, "    },")
		} else {
			fmt.Fprintln(f, "    }")
		}
	}

	fmt.Fprintln(f, "  ]")
	fmt.Fprintln(f, "}")
	return nil
}

// LoadPointerResults loads saved pscan results. Accepts both the new hex-literal
// format and any legacy file (strict JSON with string offsets).
func LoadPointerResults(path string) (*PointerResultsFile, []PointerChain, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	// Preprocess: wrap unquoted hex literals in quotes so encoding/json accepts them.
	// Legacy files (no unquoted hex) pass through unchanged.
	data = quoteHexLiterals(data)

	var prf PointerResultsFile
	if err := json.Unmarshal(data, &prf); err != nil {
		return nil, nil, fmt.Errorf("invalid results file: %v", err)
	}

	chains := make([]PointerChain, len(prf.Chains))
	for i, s := range prf.Chains {
		c, err := savedToChain(s)
		if err != nil {
			return nil, nil, err
		}
		chains[i] = c
	}
	return &prf, chains, nil
}

// --- Commands ---

var lastPscanResults []PointerResult     // in-memory last pscan output
var lastLoadedFile   *PointerResultsFile // last prload file, for expected value comparison

// prmerge <file1.json> <file2.json> [file3.json] ...
// Loads multiple chain JSON files and keeps only chains that appear in ALL of them.
// Offline equivalent of cross-session intersection — no game or pmap needed.
func cmdPointerResultsMerge(args []string) {
	if len(args) < 2 {
		fmt.Println("Usage: prmerge <file1.json> <file2.json> [file3.json] ...")
		fmt.Println("  Keeps only chains that appear in ALL files (offline cross-session intersection)")
		fmt.Println("  e.g: prmerge s1_chains.json s2_chains.json s3_chains.json")
		return
	}

	// Load first file as the base set
	_, chains0, err := LoadPointerResults(args[0])
	if err != nil {
		fmt.Printf("Load failed [%s]: %v\n", args[0], err)
		return
	}
	candidates := make(map[string]PointerChain, len(chains0))
	for _, c := range chains0 {
		candidates[c.Key()] = c
	}
	fmt.Printf("  [1/%d] %s — %d chains\n", len(args), args[0], len(candidates))

	// Intersect with each subsequent file
	for i, path := range args[1:] {
		_, chains, err := LoadPointerResults(path)
		if err != nil {
			fmt.Printf("  [%d/%d] Load failed [%s]: %v — skipping\n", i+2, len(args), path, err)
			continue
		}
		if len(chains) == 0 {
			fmt.Printf("  [%d/%d] %s — 0 chains (skipping, would kill all results)\n", i+2, len(args), path)
			continue
		}
		other := make(map[string]struct{}, len(chains))
		for _, c := range chains {
			other[c.Key()] = struct{}{}
		}
		filtered := make(map[string]PointerChain)
		for key, c := range candidates {
			if _, ok := other[key]; ok {
				filtered[key] = c
			}
		}
		candidates = filtered
		fmt.Printf("  [%d/%d] %s — %d chains → %d survivors after intersect\n",
			i+2, len(args), path, len(chains), len(candidates))
	}

	if len(candidates) == 0 {
		fmt.Println("\nNo chains survived intersection.")
		fmt.Println("Tips: try pscan with deeper depth (7), larger offset, or 'game' filter on each pmap separately")
		return
	}

	// Store as in-memory results
	results := make([]PointerResult, 0, len(candidates))
	for _, c := range candidates {
		results = append(results, PointerResult{Chain: c})
	}
	lastPscanResults = results
	fmt.Printf("\n%d chains survived — stored in memory\n", len(results))
	fmt.Println("Tip: prsave merged.json  ← save merged chains")
	fmt.Println("     prverify            ← verify against running game")
}

// prsave <file> — save last pscan results to file
func cmdPointerResultsSave(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: prsave <file.json>")
		fmt.Println("  Saves the last pscan results to a JSON file")
		return
	}
	if len(lastPscanResults) == 0 {
		fmt.Println("No pscan results in memory. Run pscan first.")
		return
	}
	gameExe := ""
	for _, m := range currentModules {
		if strings.HasSuffix(strings.ToLower(m.Name), ".exe") {
			gameExe = m.Name
			break
		}
	}
	if err := SavePointerResults(args[0], lastPscanResults, gameExe, currentIs32Bit, currentDT, currentHandle, currentModules); err != nil {
		fmt.Println("Save failed:", err)
		return
	}
	fmt.Printf("Saved %d chains to %s\n", len(lastPscanResults), args[0])
	Log.Info("prsave: saved %d chains to %s", len(lastPscanResults), args[0])
}

// prload <file> [expected_addr_hex]
// Loads saved chains. If expected_addr given, verifies each chain resolves to that address.
func cmdPointerResultsLoad(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: prload <file.json> [expected_addr_hex]")
		fmt.Println("  expected_addr: the address you found this session (e.g. 0x614DD58)")
		fmt.Println("  Chain must resolve to this address to be considered valid")
		return
	}
	prf, chains, err := LoadPointerResults(args[0])
	if err != nil {
		fmt.Println("Load failed:", err)
		return
	}

	fmt.Printf("Loaded %s\n", args[0])
	fmt.Printf("  Saved:    %s\n", prf.SavedAt.Format("2006-01-02 15:04:05"))
	fmt.Printf("  Version:  %s\n", prf.Version)
	fmt.Printf("  Game:     %s (%s)\n", prf.GameExe, map[bool]string{true: "32-bit", false: "64-bit"}[prf.Is32Bit])
	fmt.Printf("  DataType: %s\n", prf.DataType)
	fmt.Printf("  Chains:   %d\n\n", len(chains))

	lastLoadedFile = prf
	lastPscanResults = make([]PointerResult, len(chains))
	for i, c := range chains {
		lastPscanResults[i] = PointerResult{Chain: c, Label: prf.Chains[i].Label}
	}

	// If expected address given, verify chains resolve to it
	if len(args) >= 2 && currentHandle != 0 {
		expectedAddr, err := resolveAddr(args[1])
		if err != nil {
			fmt.Println("Invalid address:", err)
			return
		}
		fmt.Printf("Verifying chains resolve to 0x%X...\n", expectedAddr)
		fmt.Printf("%-5s  %-10s  %-20s  %s\n", "#", "Status", "Resolved To", "Chain")
		fmt.Println(strings.Repeat("-", 90))

		var matchedResults []PointerResult
		invalid := 0
		for i, r := range lastPscanResults {
			addr, ok := VerifyChain(currentHandle, currentModules, r.Chain, currentIs32Bit)
			if ok && addr == expectedAddr {
				fmt.Printf("%-5d  %-10s  0x%-18X  %s\n", i+1, "MATCH", addr, r.Chain.String())
				matchedResults = append(matchedResults, r)
			} else if ok {
				fmt.Printf("%-5d  %-10s  0x%-18X  %s\n", i+1, "WRONG ADDR", addr, r.Chain.String())
				invalid++
			} else {
				fmt.Printf("%-5d  %-10s  %-20s  %s\n", i+1, "BROKEN", "-", r.Chain.String())
				invalid++
			}
		}
		fmt.Printf("\n%d chains match 0x%X, %d don't\n", len(matchedResults), expectedAddr, invalid)

		// Replace in-memory results with only matched ones
		lastPscanResults = matchedResults
		if len(matchedResults) > 0 {
			fmt.Printf("\nIn-memory results updated to %d matched chains only\n", len(matchedResults))
			fmt.Println("Tip: prsave <file.json>  <- overwrite file with only working chains")
		}
		Log.Info("prload verify: %d match 0x%X, %d invalid", len(matchedResults), expectedAddr, invalid)
	} else if len(args) >= 2 && currentHandle == 0 {
		fmt.Println("Not attached to process — skipping address verification")
		fmt.Println("Use 'open <game>' first, then run: prload", args[0], args[1])
		for i, c := range chains {
			fmt.Printf("  [%d] %s\n", i+1, c.String())
		}
	} else {
		if currentHandle != 0 {
			cmdPointerResultsVerify(nil)
		} else {
			fmt.Println("Tip: prload <file> <current_addr>  to verify chains match a specific address")
			for i, c := range chains {
				fmt.Printf("  [%d] %s\n", i+1, c.String())
			}
		}
	}
	Log.Info("prload: loaded %d chains from %s", len(chains), args[0])
}

// prverify [addr] — verify chains against current process
// If addr given, only chains resolving to that address are OK
func cmdPointerResultsVerify(args []string) {
	if currentHandle == 0 {
		fmt.Println("Not attached. Use 'open <game>' first.")
		return
	}
	if len(lastPscanResults) == 0 {
		fmt.Println("No chains loaded. Run pscan or prload first.")
		return
	}

	// Optional: expected address to match against
	var expectedAddr uintptr
	hasExpected := false
	if len(args) > 0 && args[0] != "" {
		addr, err := resolveAddr(args[0])
		if err != nil {
			fmt.Println("Invalid address:", err)
			return
		}
		expectedAddr = addr
		hasExpected = true
		fmt.Printf("Verifying %d chains — must resolve to 0x%X\n", len(lastPscanResults), expectedAddr)
	} else {
		fmt.Printf("Verifying %d chains against current process...\n", len(lastPscanResults))
	}

	// Load expected values from file if available
	expectedVals := make(map[int]string)
	if lastLoadedFile != nil {
		for i, sc := range lastLoadedFile.Chains {
			if sc.ExpectedValue != "" {
				expectedVals[i] = sc.ExpectedValue
			}
		}
	}

	fmt.Printf("%-5s  %-10s  %-20s  %-12s  %-12s  %s\n", "#", "Status", "Address", "Current", "Expected", "Chain")
	fmt.Println(strings.Repeat("-", 100))

	var matchedResults []PointerResult
	ok, broken, mismatch, wrongAddr := 0, 0, 0, 0

	for i, r := range lastPscanResults {
		addr, valid := VerifyChain(currentHandle, currentModules, r.Chain, currentIs32Bit)
		if !valid {
			fmt.Printf("%-5d  %-10s  %-20s  %-12s  %-12s  %s\n", i+1, "BROKEN", "-", "-", "-", r.Chain.String())
			broken++
			continue
		}

		// If expected address given, check it matches
		if hasExpected && addr != expectedAddr {
			fmt.Printf("%-5d  %-10s  0x%-18X  %-12s  %-12s  %s\n", i+1, "WRONG ADDR", addr, "-", "-", r.Chain.String())
			wrongAddr++
			continue
		}

		currentVal, err := scanner.ReadCurrentValue(addr, currentDT)
		if err != nil {
			currentVal = "read err"
		}
		expected := expectedVals[i]
		status := "OK"
		if expected != "" && currentVal != expected {
			status = "CHANGED"
			mismatch++
		} else {
			ok++
		}
		fmt.Printf("%-5d  %-10s  0x%-18X  %-12s  %-12s  %s\n",
			i+1, status, addr, currentVal, expected, r.Chain.String())
		matchedResults = append(matchedResults, r)
	}

	fmt.Printf("\nResult: %d OK, %d changed, %d wrong addr, %d broken\n", ok, mismatch, wrongAddr, broken)

	// If address filter was used, update in-memory to matched only
	if hasExpected && len(matchedResults) > 0 {
		lastPscanResults = matchedResults
		fmt.Printf("In-memory updated to %d matched chains\n", len(matchedResults))
		fmt.Println("Tip: prsave <file.json>  <- save only working chains")
	}
	if mismatch > 0 {
		fmt.Println("CHANGED = chain resolves correctly but value is different from when you saved.")
		fmt.Println("          Did you start a new game or load a save? Value changes are normal.")
		fmt.Println("          To confirm this is the right address: prwrite <idx> <test_val> and check in game.")
	}
	if broken > 0 {
		fmt.Println("BROKEN = chain no longer resolves (game updated?)")
	}
	Log.Info("prverify: %d OK, %d changed, %d wrongAddr, %d broken", ok, mismatch, wrongAddr, broken)
}

// prlabel <index> <label> — label a chain for easier identification
func cmdPointerResultsLabel(args []string) {
	if len(args) < 2 {
		fmt.Println("Usage: prlabel <index> <label>   e.g: prlabel 1 HP")
		return
	}
	var idx int
	fmt.Sscanf(args[0], "%d", &idx)
	idx-- // 1-based to 0-based
	if idx < 0 || idx >= len(lastPscanResults) {
		fmt.Println("Invalid index")
		return
	}
	label := strings.Join(args[1:], " ")
	lastPscanResults[idx].Label = label
	fmt.Printf("Chain %d labeled as '%s'\n", idx+1, label)
}

// prfreeze <index> <value> — freeze a verified chain's address
func cmdPointerResultsFreeze(args []string) {
	if len(args) < 2 {
		fmt.Println("Usage: prfreeze <index> <value>   e.g: prfreeze 1 999")
		return
	}
	if currentHandle == 0 {
		fmt.Println("Not attached")
		return
	}
	var idx int
	fmt.Sscanf(args[0], "%d", &idx)
	idx--
	if idx < 0 || idx >= len(lastPscanResults) {
		fmt.Println("Invalid index")
		return
	}
	addr, ok := VerifyChain(currentHandle, currentModules, lastPscanResults[idx].Chain, currentIs32Bit)
	if !ok {
		fmt.Printf("Chain %d is broken — cannot freeze\n", idx+1)
		return
	}
	val, err := encodeValue(currentDT, args[1])
	if err != nil {
		fmt.Println("Invalid value:", err)
		return
	}
	id := freezer.Add(addr, val, lastPscanResults[idx].Chain.String())
	fmt.Printf("Freezing chain %d -> 0x%X = %s (freeze id=%d)\n", idx+1, addr, args[1], id)
	Log.Info("prfreeze: chain %d addr=0x%X val=%s freeze_id=%d", idx+1, addr, args[1], id)
}

// prwrite <index> <value> — write to a verified chain's address once
func cmdPointerResultsWrite(args []string) {
	if len(args) < 2 {
		fmt.Println("Usage: prwrite <index> <value>   e.g: prwrite 1 999")
		return
	}
	if currentHandle == 0 {
		fmt.Println("Not attached")
		return
	}
	var idx int
	fmt.Sscanf(args[0], "%d", &idx)
	idx--
	if idx < 0 || idx >= len(lastPscanResults) {
		fmt.Println("Invalid index")
		return
	}
	addr, ok := VerifyChain(currentHandle, currentModules, lastPscanResults[idx].Chain, currentIs32Bit)
	if !ok {
		fmt.Printf("Chain %d is broken\n", idx+1)
		return
	}
	val, err := encodeValue(currentDT, args[1])
	if err != nil {
		fmt.Println("Invalid value:", err)
		return
	}
	if err := WriteMemory(currentHandle, addr, val); err != nil {
		fmt.Println("Write failed:", err)
		return
	}
	fmt.Printf("Written %s to chain %d -> 0x%X\n", args[1], idx+1, addr)
	Log.Info("prwrite: chain %d addr=0x%X val=%s", idx+1, addr, args[1])
}

// prlist [addr|ok] — list current in-memory pointer results.
// No arg: list all chains.
// "ok": only chains that currently resolve (any address).
// hex address: only chains that resolve to exactly that address.
func cmdPointerResultsList(args []string) {
	if len(lastPscanResults) == 0 {
		fmt.Println("No results. Run pscan or prload first.")
		return
	}

	// Determine filter mode
	filterAddr := false
	filterOK := false
	var wantAddr uintptr

	if len(args) > 0 {
		if strings.ToLower(args[0]) == "ok" {
			filterOK = true
		} else {
			addr, err := resolveAddr(args[0])
			if err != nil {
				fmt.Println("Invalid argument: expected a hex address or 'ok'")
				return
			}
			filterAddr = true
			wantAddr = addr
		}
		if (filterAddr || filterOK) && currentHandle == 0 {
			fmt.Println("Not attached to process — cannot verify chains. Use 'open <game>' first.")
			return
		}
	}

	attached := currentHandle != 0
	fmt.Printf("%-5s  %-20s  %-10s  %-14s  %s\n", "#", "Address", "Value", "Label", "Chain")
	fmt.Println(strings.Repeat("-", 110))

	shown := 0
	for i, r := range lastPscanResults {
		if filterAddr || filterOK {
			addr, valid := VerifyChain(currentHandle, currentModules, r.Chain, currentIs32Bit)
			if !valid { continue }
			if filterAddr && addr != wantAddr { continue }
			val := "-"
			if attached {
				if v, err := scanner.ReadCurrentValue(addr, currentDT); err == nil { val = v }
			}
			fmt.Printf("%-5d  0x%-18X  %-10s  %-14s  %s\n", i+1, addr, val, r.Label, r.Chain.String())
		} else if attached {
			addr, valid := VerifyChain(currentHandle, currentModules, r.Chain, currentIs32Bit)
			val := "BROKEN"
			addrStr := "-"
			if valid {
				addrStr = fmt.Sprintf("0x%X", addr)
				if v, err := scanner.ReadCurrentValue(addr, currentDT); err == nil { val = v }
			}
			fmt.Printf("%-5d  %-20s  %-10s  %-14s  %s\n", i+1, addrStr, val, r.Label, r.Chain.String())
		} else {
			fmt.Printf("%-5d  %-20s  %-10s  %-14s  %s\n", i+1, "-", "-", r.Label, r.Chain.String())
		}
		shown++
	}

	fmt.Printf("\n%d/%d chains shown\n", shown, len(lastPscanResults))
}

// --- Wire into cmdPointerScan to auto-store results ---
// results = all candidates (uncapped). maxResults = how many to keep after verifying.
// If attached: verifies each chain, shows only OK ones up to maxResults.
// If not attached: shows first maxResults unverified.
func storeAndPrintResults(results []PointerResult, handle windows.Handle, maxResults int) {
	fmt.Println()

	if handle != 0 && len(results) > 0 {
		fmt.Printf("Verifying %d candidates — keeping first %d that resolve...\n", len(results), maxResults)
		var okResults []PointerResult
		broken := 0
		for _, r := range results {
			addr, valid := VerifyChain(handle, currentModules, r.Chain, currentIs32Bit)
			if !valid {
				broken++
				continue
			}
			val, err := scanner.ReadCurrentValue(addr, currentDT)
			valStr := ""
			if err == nil {
				valStr = fmt.Sprintf(" = %s", val)
			}
			idx := len(okResults) + 1
			fmt.Printf("  [%d] OK -> 0x%X%s  %s\n", idx, addr, valStr, r.Chain.String())
			okResults = append(okResults, r)
			if len(okResults) >= maxResults {
				break
			}
		}
		fmt.Printf("\nShowing %d OK chains (%d broken/skipped from %d total)\n", len(okResults), broken, len(results))
		lastPscanResults = okResults
	} else {
		// Not attached — just show first maxResults unverified
		shown := results
		if len(shown) > maxResults {
			shown = shown[:maxResults]
		}
		for i, r := range shown {
			fmt.Printf("[%d] %s\n", i+1, r.Chain.String())
		}
		lastPscanResults = shown
	}

	fmt.Printf("\nTip: prsave results.json  <- save these chains\n")
	fmt.Printf("     prverify             <- re-verify after game restart\n")
}
