// Package config reads and checks elprovo.toml and proof files, as section 1
// of SPEC.md defines them.
package config

import (
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// MaxFileSize is the largest elprovo.toml or proof file that Elprovo reads.
const MaxFileSize = 64 << 10

// RepoFile is the path of the repository file, relative to the repository root.
const RepoFile = "elprovo.toml"

// ProofDir is the directory that holds proof files and their helper files.
const ProofDir = "proofs"

const maxClaimLength = 200

// Repo is a checked elprovo.toml.
type Repo struct {
	Start        string
	Ready        string
	Stop         string
	Env          map[string]string
	PassEnv      []string
	StartTimeout time.Duration
	ReadyTimeout time.Duration
	StopTimeout  time.Duration
}

// Before says what a proof must do on the base commit.
type Before string

const (
	BeforeFail Before = "fail"
	BeforeSkip Before = "skip"
)

// Proof is a checked proof file.
type Proof struct {
	File    string
	Name    string
	Claim   string
	Run     string
	Before  Before
	Timeout time.Duration
}

var (
	proofNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	envNamePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	durationPattern  = regexp.MustCompile(`^([1-9][0-9]*)([smh])$`)
)

// ProofName reports whether file, a slash-separated path from the repository
// root, is a proof file, and returns its name. A file directly in proofs/
// that ends in .toml but has an invalid name is an error, not a helper file,
// and so is a YAML file directly in proofs/.
func ProofName(file string) (string, bool, error) {
	dir, base := path.Split(file)
	if dir != ProofDir+"/" {
		return "", false, nil
	}
	ext := path.Ext(base)
	switch strings.ToLower(ext) {
	case ".yml", ".yaml":
		return "", false, fmt.Errorf("%s: Elprovo reads proof files that end in .toml. Rename a proof file to .toml, or move a YAML helper file into a subdirectory of %s/", EscapeControl(file), ProofDir)
	case ".toml":
	default:
		return "", false, nil
	}
	if ext != ".toml" {
		return "", false, fmt.Errorf("%s: a proof file name must end in .toml, in lowercase", EscapeControl(file))
	}
	name := strings.TrimSuffix(base, ext)
	if !proofNamePattern.MatchString(name) {
		return "", false, fmt.Errorf("%s: the name before .toml must match [a-z0-9][a-z0-9-]*", EscapeControl(file))
	}
	return name, true, nil
}

type rawRepo struct {
	Version  *int              `toml:"version"`
	Start    string            `toml:"start"`
	Ready    string            `toml:"ready"`
	Stop     string            `toml:"stop"`
	Env      map[string]string `toml:"env"`
	PassEnv  []string          `toml:"pass_env"`
	Timeouts struct {
		Start string `toml:"start"`
		Ready string `toml:"ready"`
		Stop  string `toml:"stop"`
	} `toml:"timeouts"`
}

// ParseRepo checks the content of an elprovo.toml. name is the file's name in
// error messages.
func ParseRepo(name string, data []byte) (Repo, error) {
	var raw rawRepo
	if err := decode(data, &raw); err != nil {
		return Repo{}, fmt.Errorf("%s: %w", name, err)
	}
	var problems []string
	switch {
	case raw.Version == nil:
		problems = append(problems, "version is missing")
	case *raw.Version != 1:
		problems = append(problems, fmt.Sprintf("version %d is not supported; this release reads version 1", *raw.Version))
	}
	problems = requireCommand(problems, "start", raw.Start)
	problems = requireCommand(problems, "ready", raw.Ready)
	for envName := range raw.Env {
		problems = checkEnvName(problems, "env", envName)
	}
	for _, envName := range raw.PassEnv {
		problems = checkEnvName(problems, "pass_env", envName)
	}

	repo := Repo{
		Start:   raw.Start,
		Ready:   raw.Ready,
		Stop:    strings.TrimSpace(raw.Stop),
		Env:     raw.Env,
		PassEnv: raw.PassEnv,
	}
	problems = parseDuration(problems, "timeouts.start", raw.Timeouts.Start, 10*time.Minute, &repo.StartTimeout)
	problems = parseDuration(problems, "timeouts.ready", raw.Timeouts.Ready, 5*time.Minute, &repo.ReadyTimeout)
	problems = parseDuration(problems, "timeouts.stop", raw.Timeouts.Stop, 2*time.Minute, &repo.StopTimeout)
	if len(problems) > 0 {
		return Repo{}, fmt.Errorf("%s: %s", name, strings.Join(problems, "; "))
	}
	return repo, nil
}

type rawProof struct {
	Claim   string `toml:"claim"`
	Run     string `toml:"run"`
	Before  string `toml:"before"`
	Timeout string `toml:"timeout"`
}

// ParseProof checks the content of the proof file at file, a slash-separated
// path from the repository root.
func ParseProof(file string, data []byte) (Proof, error) {
	name, ok, err := ProofName(file)
	if err != nil {
		return Proof{}, err
	}
	if !ok {
		return Proof{}, fmt.Errorf("%s: a proof file must be directly in %s/ and end in .toml", file, ProofDir)
	}
	var raw rawProof
	if err := decode(data, &raw); err != nil {
		return Proof{}, fmt.Errorf("%s: %w", file, err)
	}
	var problems []string
	claim := strings.TrimSpace(raw.Claim)
	switch {
	case claim == "":
		problems = append(problems, "claim is missing")
	case strings.ContainsFunc(claim, isControl):
		problems = append(problems, "claim must be one line, without control characters")
	case utf8.RuneCountInString(claim) > maxClaimLength:
		problems = append(problems, fmt.Sprintf("claim has %d characters; the limit is %d", utf8.RuneCountInString(claim), maxClaimLength))
	}
	problems = requireCommand(problems, "run", raw.Run)

	proof := Proof{File: file, Name: name, Claim: claim, Run: raw.Run}
	switch Before(raw.Before) {
	case "", BeforeFail:
		proof.Before = BeforeFail
	case BeforeSkip:
		proof.Before = BeforeSkip
	default:
		problems = append(problems, fmt.Sprintf("before is %q; it must be fail or skip", raw.Before))
	}
	problems = parseDuration(problems, "timeout", raw.Timeout, 10*time.Minute, &proof.Timeout)
	if len(problems) > 0 {
		return Proof{}, fmt.Errorf("%s: %s", file, strings.Join(problems, "; "))
	}
	return proof, nil
}

// decode reads a TOML document into v and rejects keys that v has no field for.
func decode(data []byte, v any) error {
	if len(data) > MaxFileSize {
		return fmt.Errorf("the file has %d bytes; the limit is %d", len(data), MaxFileSize)
	}
	meta, err := toml.Decode(string(data), v)
	if err != nil {
		// The message can quote the file, which comes from the change, so
		// it must not reach a terminal with control characters in it.
		return errors.New(EscapeControl(err.Error()))
	}
	if len(meta.Keys()) == 0 {
		return errors.New("the file is empty")
	}
	t := reflect.TypeOf(v).Elem()
	if unknown := unknownKeys(meta, t); len(unknown) > 0 {
		noun := "field"
		if len(unknown) > 1 {
			noun = "fields"
		}
		return fmt.Errorf("unknown %s %s", noun, strings.Join(unknown, ", "))
	}
	// The decoder leaves a map field empty, without an error, when the file
	// gives it a value that is not a table. Dotted keys, such as env.PORT,
	// make a table whose type is empty.
	for i := range t.NumField() {
		field := t.Field(i)
		name := field.Tag.Get("toml")
		if kind := meta.Type(name); field.Type.Kind() == reflect.Map && kind != "" && kind != "Hash" {
			return fmt.Errorf("%s must be a table, such as %s.NAME = \"value\"", name, name)
		}
	}
	return nil
}

// unknownKeys lists, quoted, the keys of the file that no field of t has as
// its exact toml tag. The decoder also matches a key to a field in another
// case, so MetaData.Undecoded would let CLAIM set claim. A key under a map,
// such as a name in env, is not checked.
func unknownKeys(meta toml.MetaData, t reflect.Type) []string {
	var unknown []string
	for _, key := range meta.Keys() {
		parent := t
		for i, part := range key {
			if parent.Kind() != reflect.Struct {
				break
			}
			field, ok := fieldWithTag(parent, part)
			if !ok {
				// The keys under an unknown table would repeat its error, so
				// only the table's own key reports it.
				if i == len(key)-1 {
					unknown = append(unknown, strconv.Quote(key.String()))
				}
				break
			}
			parent = field.Type
		}
	}
	return unknown
}

func fieldWithTag(t reflect.Type, tag string) (reflect.StructField, bool) {
	for i := range t.NumField() {
		if field := t.Field(i); field.Tag.Get("toml") == tag {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

// isControl reports whether r is a control character or a line or paragraph
// separator, which a terminal or a viewer can act on.
func isControl(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp)
}

// EscapeControl writes every control character in s, except line breaks
// and tabs, as a Go escape sequence, so that text from a change cannot act on
// a terminal.
func EscapeControl(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isControl(r) && r != '\n' && r != '\t' {
			quoted := strconv.QuoteRuneToASCII(r)
			b.WriteString(quoted[1 : len(quoted)-1])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func requireCommand(problems []string, field, command string) []string {
	if strings.TrimSpace(command) == "" {
		return append(problems, field+" is missing")
	}
	return problems
}

func checkEnvName(problems []string, field, name string) []string {
	switch {
	case !envNamePattern.MatchString(name):
		return append(problems, fmt.Sprintf("%s name %q must match [A-Za-z_][A-Za-z0-9_]*", field, name))
	case strings.HasPrefix(name, "ELPROVO_"):
		return append(problems, fmt.Sprintf("%s name %q must not start with ELPROVO_", field, name))
	}
	return problems
}

// parseDuration parses a duration from section 1.3 of SPEC.md into *out, or
// sets the default when text is empty.
func parseDuration(problems []string, field, text string, fallback time.Duration, out *time.Duration) []string {
	if text == "" {
		*out = fallback
		return problems
	}
	match := durationPattern.FindStringSubmatch(text)
	if match == nil {
		return append(problems, fmt.Sprintf("%s is %q; write a whole number followed by s, m or h, such as 90s", field, text))
	}
	count, err := strconv.Atoi(match[1])
	if err != nil {
		return append(problems, fmt.Sprintf("%s is %q; the number is too large", field, text))
	}
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}[match[2]]
	if count > int(time.Hour/unit) {
		return append(problems, fmt.Sprintf("%s is %q; the limit is 1h", field, text))
	}
	*out = time.Duration(count) * unit
	return problems
}
