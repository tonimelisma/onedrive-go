package localpath

import (
	"errors"
	"io/fs"
	"os"
	"strings"
)

// callError is the failure of one filesystem call on a path: "<verb> <path>:
// <reason>", the path named once, as the caller gave it. err is the call's
// own error, kept whole, so errors.Is and errors.As answer on a callError as
// they do on it, and its *fs.PathError or *os.LinkError still holds the
// absolute path the call reached. An os error on that very path would name
// it a second time, so only its reason is printed (cause).
type callError struct {
	verb string
	// object is the path, "<from> to <to>" for a rename, or "" for a step
	// that follows a failure on the same path, which named it already.
	object string
	err    error
	cause  error // what reads after the path: err, or the reason it holds
}

func (e *callError) Error() string {
	if e.object == "" {
		return e.verb + ": " + e.cause.Error()
	}

	return e.verb + " " + e.object + ": " + e.cause.Error()
}

func (e *callError) Unwrap() error { return e.err }

// failure is err, the failure of verb on object, the path as the caller gave
// it ("<from> to <to>" for a rename). reached is the path the call reached,
// or the two of a rename, which err names too.
func failure(verb, object string, err error, reached ...string) error {
	return &callError{verb: verb, object: object, err: err, cause: reasonOf(err, reached)}
}

// reasonOf is the part of err that reads after the path a failure names: the
// reason an os error on reached holds, or err itself, an os error that names
// another path (a parent MkdirAll could not create) among them.
func reasonOf(err error, reached []string) error {
	if pathErr, ok := errors.AsType[*fs.PathError](err); ok && len(reached) == 1 && pathErr.Path == reached[0] {
		return pathErr.Err
	}
	if linkErr, ok := errors.AsType[*os.LinkError](err); ok && len(reached) == 2 &&
		linkErr.Old == reached[0] && linkErr.New == reached[1] {
		return linkErr.Err
	}

	return err
}

// osErrorPath is the path err, an os call's error, names, or "" when it
// names none. A temp file's or folder's failure names the name the call made
// up in the folder, which tells the user nothing the folder does not.
func osErrorPath(err error) string {
	if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
		return pathErr.Path
	}

	return ""
}

// WithoutPath is err's text with every local path in it left out: each
// failure of this package keeps its verb and its reason ("hashing: opening:
// permission denied" for "hashing: opening a.txt: permission denied"), and an
// *fs.PathError or *os.LinkError keeps its operation and its reason. A line
// that names the path itself, such as a folder transfer's failure line,
// prints its cause with it, so the path is named once. A wrapper whose text
// does not hold the text of the error it wraps keeps its own words. A nil err
// is "".
func WithoutPath(err error) string {
	if err == nil {
		return ""
	}

	text := err.Error()
	for _, named := range namingErrors(err, nil) {
		text = strings.Replace(text, named.Error(), withoutPathOf(named), 1)
	}

	return text
}

// namingErrors appends to found the errors in err's tree that name a local
// path, outermost first, without looking inside them: withoutPathOf renders
// what each holds.
//
//nolint:errorlint // walks the tree one error at a time; errors.As would find the same error below every wrapper
func namingErrors(err error, found []error) []error {
	switch e := err.(type) {
	case *callError, *fs.PathError, *os.LinkError:
		return append(found, err)
	case interface{ Unwrap() []error }:
		for _, wrapped := range e.Unwrap() {
			found = namingErrors(wrapped, found)
		}

		return found
	case interface{ Unwrap() error }:
		if wrapped := e.Unwrap(); wrapped != nil {
			return namingErrors(wrapped, found)
		}
	}

	return found
}

// withoutPathOf is the text of named, an error namingErrors found, without
// the path it names.
//
//nolint:errorlint // named is one error namingErrors found, never a wrapper of one
func withoutPathOf(named error) string {
	switch e := named.(type) {
	case *callError:
		return e.verb + ": " + WithoutPath(e.cause)
	case *fs.PathError:
		return e.Op + ": " + WithoutPath(e.Err)
	case *os.LinkError:
		return e.Op + ": " + WithoutPath(e.Err)
	default:
		return named.Error()
	}
}
