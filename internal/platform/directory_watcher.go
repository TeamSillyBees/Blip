package platform

import "github.com/fsnotify/fsnotify"

// DirectoryWatcher keeps draining native notifications while its caller adds
// watches or processes files. On Windows, Add and event delivery share a
// backend goroutine. A full native event channel can otherwise block Add.
type DirectoryWatcher struct {
	*fsnotify.Watcher
	Events <-chan fsnotify.Event
	Errors <-chan error
}

func NewDirectoryWatcher() (*DirectoryWatcher, error) {
	native, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	events := make(chan fsnotify.Event, 256)
	gaps := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(gaps)
		report := func(err error) {
			select {
			case gaps <- err:
			default: // One pending error already requests a reconciliation.
			}
		}
		for {
			select {
			case e, ok := <-native.Events:
				if !ok {
					return
				}
				select {
				case events <- e:
				default:
					report(fsnotify.ErrEventOverflow)
				}
			case err, ok := <-native.Errors:
				if !ok {
					return
				}
				report(err)
			}
		}
	}()
	return &DirectoryWatcher{Watcher: native, Events: events, Errors: gaps}, nil
}
