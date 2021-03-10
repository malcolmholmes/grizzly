package grizzly

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/google/go-jsonnet"
	"github.com/grafana/grizzly/pkg/term"
	"github.com/grafana/tanka/pkg/jsonnet/native"
	"github.com/pmezard/go-difflib/difflib"
	"golang.org/x/crypto/ssh/terminal"
	"gopkg.in/fsnotify.v1"
)

var interactive = terminal.IsTerminal(int(os.Stdout.Fd()))

func isMultiResource(handler Handler) bool {
	_, ok := handler.(MultiResourceHandler)
	return ok
}

// Get retrieves a resource from a remote endpoint using its UID
func Get(config Config, UID string) error {
	count := strings.Count(UID, ".")
	var handlerName, resourceID string
	if count == 1 {
		parts := strings.SplitN(UID, ".", 2)
		handlerName = parts[0]
		resourceID = parts[1]
	} else if count == 2 {
		parts := strings.SplitN(UID, ".", 3)
		handlerName = parts[0] + "." + parts[1]
		resourceID = parts[2]

	} else {
		return fmt.Errorf("UID must be <provider>.<uid>: %s", UID)
	}

	handler, err := config.Registry.GetHandler(handlerName)
	if err != nil {
		return err
	}

	resource, err := handler.GetByUID(resourceID)
	if err != nil {
		return err
	}

	resource = handler.Unprepare(*resource)
	rep, err := resource.GetRepresentation()
	if err != nil {
		return err
	}

	fmt.Println(rep)
	return nil
}

// List outputs the keys resources found in resulting json.
func List(config Config, resources Resources) error {
	f := "%s\t%s\t%s\n"
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 4, ' ', 0)

	fmt.Fprintf(w, f, "API VERSION", "KIND", "UID")
	for handler, resourceList := range resources {
		for _, r := range resourceList {
			fmt.Fprintf(w, f, handler.APIVersion(), handler.Kind(), r.UID)
		}
	}
	return w.Flush()
}

func getPrivateElementsScript(jsonnetFile string, handlers []Handler) string {
	const script = `
    local src = import '%s';
    src + {
    %s
    }
	`
	handlerStrings := []string{}
	for _, handler := range handlers {
		for _, jsonPath := range handler.GetJSONPaths() {
			handlerStrings = append(handlerStrings, fmt.Sprintf("  %s+::: {},", jsonPath))
		}
	}
	return fmt.Sprintf(script, jsonnetFile, strings.Join(handlerStrings, "\n"))
}

// Parse evaluates a jsonnet file and parses it into an object tree
func Parse(config Config, jsonnetFile string, targets []string) (Resources, error) {

	script := getPrivateElementsScript(jsonnetFile, config.Registry.Handlers)
	vm := jsonnet.MakeVM()
	vm.Importer(newExtendedImporter([]string{"vendor", "lib", "."}))
	for _, nf := range native.Funcs() {
		vm.NativeFunction(nf)
	}

	result, err := vm.EvaluateSnippet(jsonnetFile, script)
	if err != nil {
		return nil, err
	}

	msi := map[string]interface{}{}
	if err := json.Unmarshal([]byte(result), &msi); err != nil {
		return nil, err
	}

	resources := Resources{}

	for k, v := range msi {
		handler, err := config.Registry.GetHandler(k)
		if err != nil {
			fmt.Println("Skipping unregistered path", k)
			continue
		}
		handlerResources, err := handler.Parse(k, v)
		if err != nil {
			return nil, err
		}
		resourceList, ok := resources[handler]
		if !ok {
			resourceList = ResourceList{}
		}
		for kk, resource := range handlerResources {
			if resource.MatchesTarget(targets) {
				resourceList[kk] = resource
			}
		}
		resources[handler] = resourceList
	}
	return resources, nil
}

// Show displays resources
func Show(config Config, resources Resources) error {

	var items []term.PageItem
	for handler, resourceList := range resources {
		for _, resource := range resourceList {
			resource = *(handler.Unprepare(resource))

			rep, err := resource.GetRepresentation()
			if err != nil {
				return err
			}
			if interactive {
				items = append(items, term.PageItem{
					Name:    fmt.Sprintf("%s/%s", resource.Kind(), resource.UID),
					Content: rep,
				})
			} else {
				fmt.Printf("%s/%s:\n", resource.Kind(), resource.UID)
				fmt.Println(rep)
			}
		}
	}
	if interactive {
		return term.Page(items)
	}
	return nil
}

// Diff compares resources to those at the endpoints
func Diff(config Config, resources Resources) error {

	for handler, resourceList := range resources {
		if isMultiResource(handler) {
			multiHandler := handler.(MultiResourceHandler)
			multiHandler.Diff(config.Notifier, resourceList)
			continue
		}

		for _, resource := range resourceList {
			local, err := resource.GetRepresentation()
			if err != nil {
				return nil
			}
			resource = *handler.Unprepare(resource)
			uid := resource.UID
			remote, err := handler.GetRemote(resource.UID)
			if err == ErrNotFound {
				config.Notifier.NotFound(resource)
				continue
			}
			if err != nil {
				return fmt.Errorf("Error retrieving resource from %s %s: %v", resource.Kind(), uid, err)
			}
			remote = handler.Unprepare(*remote)
			remoteRepresentation, err := (*remote).GetRepresentation()
			if err != nil {
				return err
			}

			if local == remoteRepresentation {
				config.Notifier.NoChanges(resource)
			} else {
				diff := difflib.UnifiedDiff{
					A:        difflib.SplitLines(remoteRepresentation),
					B:        difflib.SplitLines(local),
					FromFile: "Remote",
					ToFile:   "Local",
					Context:  3,
				}
				difference, _ := difflib.GetUnifiedDiffString(diff)
				config.Notifier.HasChanges(resource, difference)
			}
		}
	}
	return nil
}

// Apply pushes resources to endpoints
func Apply(config Config, resources Resources) error {
	for handler, resourceList := range resources {
		if isMultiResource(handler) {
			multiHandler := handler.(MultiResourceHandler)
			err := multiHandler.Apply(config.Notifier, resourceList)
			if err != nil {
				return err
			}
			continue
		}
		for _, resource := range resourceList {
			existingResource, err := handler.GetRemote(resource.UID)
			if err == ErrNotFound {

				err := handler.Add(resource)
				if err != nil {
					return err
				}
				config.Notifier.Added(resource)
				continue
			} else if err != nil {
				return err
			}
			resourceRepresentation, err := resource.GetRepresentation()
			if err != nil {
				return err
			}
			resource = *handler.Prepare(*existingResource, resource)
			existingResource = handler.Unprepare(*existingResource)
			existingResourceRepresentation, err := existingResource.GetRepresentation()
			if err != nil {
				return nil
			}
			if resourceRepresentation == existingResourceRepresentation {
				config.Notifier.NoChanges(resource)
			} else {
				err = handler.Update(*existingResource, resource)
				if err != nil {
					return err
				}
				config.Notifier.Updated(resource)
			}
		}
	}
	return nil
}

// Preview pushes resources to endpoints as previews, if supported
func Preview(config Config, resources Resources, opts *PreviewOpts) error {
	for handler, resourceList := range resources {
		for _, resource := range resourceList {
			previewHandler, ok := handler.(PreviewHandler)
			if !ok {
				tmpResource := Resource{
					JSONPath: "",
					UID:      resource.UID,
					Handler:  handler,
				}
				config.Notifier.NotSupported(tmpResource, "preview")
				return nil
			}
			err := previewHandler.Preview(resource, config.Notifier, opts)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Parser encapsulates the action of parsing a resource (jsonnet or otherwise)
type Parser interface {
	Name() string
	Parse(config Config) (Resources, error)
}

// Watch watches a directory for changes then pushes Jsonnet resource to endpoints
// when changes are noticed
func Watch(config Config, watchDir string, parser Parser) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	done := make(chan bool)
	go func() {
		log.Println("Watching for changes")
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					log.Println("Changes detected. Applying", parser.Name())
					resources, err := parser.Parse(config)
					if err != nil {
						log.Println("Error: ", err)
					}
					err = Apply(config, resources)
					if err != nil {
						log.Println("Error: ", err)
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("error:", err)
			}
		}
	}()

	err = watcher.Add(watchDir)
	if err != nil {
		return err
	}
	<-done
	return nil
}

// Listen waits for remote changes to a resource and saves them to disk
func Listen(config Config, UID, filename string) error {
	count := strings.Count(UID, ".")
	var handlerName, resourceID string
	if count == 1 {
		parts := strings.SplitN(UID, ".", 2)
		handlerName = parts[0]
		resourceID = parts[1]
	} else if count == 2 {
		parts := strings.SplitN(UID, ".", 3)
		handlerName = parts[0] + "." + parts[1]
		resourceID = parts[2]

	} else {
		return fmt.Errorf("UID must be <provider>.<uid>: %s", UID)
	}

	handler, err := config.Registry.GetHandler(handlerName)
	if err != nil {
		return err
	}
	listenHandler, ok := handler.(ListenHandler)
	if !ok {
		tmpResource := Resource{
			JSONPath: "",
			UID:      resourceID,
			Handler:  handler,
		}
		config.Notifier.NotSupported(tmpResource, "listen")
		return nil
	}
	return listenHandler.Listen(config.Notifier, resourceID, filename)
}

// Export renders Jsonnet resources then saves them to a directory
func Export(config Config, exportDir string, resources Resources) error {
	if _, err := os.Stat(exportDir); os.IsNotExist(err) {
		err = os.Mkdir(exportDir, 0755)
		if err != nil {
			return err
		}
	}

	for handler, resourceList := range resources {
		for _, resource := range resourceList {
			updatedResource, err := resource.GetRepresentation()
			if err != nil {
				return err
			}
			extension := handler.GetExtension()
			dir := fmt.Sprintf("%s/%s", exportDir, resource.Kind())
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				err = os.Mkdir(dir, 0755)
				if err != nil {
					return err
				}
			}
			path := fmt.Sprintf("%s/%s.%s", dir, resource.UID, extension)

			existingResourceBytes, err := ioutil.ReadFile(path)
			isNotExist := os.IsNotExist(err)
			if err != nil && !isNotExist {
				return err
			}
			existingResource := string(existingResourceBytes)
			if existingResource == updatedResource {
				config.Notifier.NoChanges(resource)
			} else {
				err = ioutil.WriteFile(path, []byte(updatedResource), 0644)
				if err != nil {
					return err
				}
				if isNotExist {
					config.Notifier.Added(resource)
				} else {
					config.Notifier.Updated(resource)
				}
			}
		}
	}
	return nil
}
