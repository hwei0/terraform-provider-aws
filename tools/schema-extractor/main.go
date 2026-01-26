package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sort"
	"unsafe"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-provider-aws/internal/conns"
	"github.com/hashicorp/terraform-provider-aws/internal/provider/sdkv2"
)

type ResourceInfo struct {
	ResourceType         string                `json:"resource_type"`
	CreateWithoutTimeout string                `json:"create_without_timeout,omitempty"`
	CreateContext        string                `json:"create_context,omitempty"`
	Create               string                `json:"create,omitempty"`
	ReadWithoutTimeout   string                `json:"read_without_timeout,omitempty"`
	ReadContext          string                `json:"read_context,omitempty"`
	Read                 string                `json:"read,omitempty"`
	UpdateWithoutTimeout string                `json:"update_without_timeout,omitempty"`
	UpdateContext        string                `json:"update_context,omitempty"`
	Update               string                `json:"update,omitempty"`
	DeleteWithoutTimeout string                `json:"delete_without_timeout,omitempty"`
	DeleteContext        string                `json:"delete_context,omitempty"`
	Delete               string                `json:"delete,omitempty"`
	Schema               map[string]SchemaInfo `json:"schema"`
	Timeouts             *TimeoutsInfo         `json:"timeouts,omitempty"`
	DeprecationMsg       string                `json:"deprecation_message,omitempty"`
	Description          string                `json:"description,omitempty"`
	HasImporter          bool                  `json:"has_importer"`
	HasCustomizeDiff     bool                  `json:"has_customize_diff"`
}

type SchemaInfo struct {
	Type          string      `json:"type"`
	Optional      bool        `json:"optional,omitempty"`
	Required      bool        `json:"required,omitempty"`
	Computed      bool        `json:"computed,omitempty"`
	ForceNew      bool        `json:"force_new,omitempty"`
	Description   string      `json:"description,omitempty"`
	Deprecated    string      `json:"deprecated,omitempty"`
	ConflictsWith []string    `json:"conflicts_with,omitempty"`
	ExactlyOneOf  []string    `json:"exactly_one_of,omitempty"`
	AtLeastOneOf  []string    `json:"at_least_one_of,omitempty"`
	RequiredWith  []string    `json:"required_with,omitempty"`
	Default       interface{} `json:"default,omitempty"`
	Sensitive     bool        `json:"sensitive,omitempty"`
	MaxItems      int         `json:"max_items,omitempty"`
	MinItems      int         `json:"min_items,omitempty"`
	Elem          interface{} `json:"elem,omitempty"`
}

type TimeoutsInfo struct {
	Create string `json:"create,omitempty"`
	Read   string `json:"read,omitempty"`
	Update string `json:"update,omitempty"`
	Delete string `json:"delete,omitempty"`
}

// getServicePackagesViaReflection gets service packages by creating a provider
// and extracting them from the Meta using reflection.
//
// ALTERNATIVE: Instead of using reflection, you can create a simple export file:
//
//	File: internal/provider/sdkv2/service_packages_export.go
//	Content:
//	  package sdkv2
//	  import (
//	      "context"
//	      "github.com/hashicorp/terraform-provider-aws/internal/conns"
//	  )
//	  func ServicePackages(ctx context.Context) []conns.ServicePackage {
//	      return servicePackages(ctx)
//	  }
//	Then replace this function with: return sdkv2.ServicePackages(ctx), nil
//
// The export file approach is:
//   - Cleaner: 3-line wrapper vs complex reflection code
//   - Safer: No unsafe package needed
//   - More maintainable: Explicit intent, won't break on internal changes
//   - Standard Go practice: Common pattern for exposing internal APIs to tools

// For some reason, doing it this way results in 3 services having a "region" schema not being picked up, while reflection does. WHY?
func getServicePackagesViaReflection(ctx context.Context) ([]conns.ServicePackage, error) {
	return sdkv2.ServicePackages(ctx), nil
}

func getServicePackagesViaReflectionOld(ctx context.Context) ([]conns.ServicePackage, error) {
	fmt.Println("Using reflection to access service packages...")

	// Create a provider instance which will initialize service packages
	provider, err := sdkv2.NewProvider(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider: %w", err)
	}

	// Get the Meta which contains the service packages
	meta := provider.Meta()
	if meta == nil {
		return nil, fmt.Errorf("provider meta is nil")
	}

	// Cast to AWSClient
	client, ok := meta.(*conns.AWSClient)
	if !ok {
		return nil, fmt.Errorf("meta is not *conns.AWSClient, got %T", meta)
	}

	// Use reflection to access the unexported servicePackages field
	clientValue := reflect.ValueOf(client).Elem()

	// Look for the servicePackages field (it's a map[string]conns.ServicePackage)
	var servicePackagesField reflect.Value
	found := false

	for i := 0; i < clientValue.NumField(); i++ {
		field := clientValue.Field(i)
		fieldType := clientValue.Type().Field(i)

		// Check if this is a map with ServicePackage values
		if field.Kind() == reflect.Map && field.Type().Elem().String() == "conns.ServicePackage" {
			servicePackagesField = field
			found = true
			fmt.Printf("Found service packages field: %s\n", fieldType.Name)
			break
		}
	}

	if !found {
		return nil, fmt.Errorf("could not find service packages map in AWSClient")
	}

	// If the field is unexported, we need to use unsafe to access it
	if !servicePackagesField.CanInterface() {
		// Create a new value that we can interface with using unsafe
		servicePackagesField = reflect.NewAt(
			servicePackagesField.Type(),
			unsafe.Pointer(servicePackagesField.UnsafeAddr()),
		).Elem()
	}

	// Extract the service packages from the map
	packages := make([]conns.ServicePackage, 0, servicePackagesField.Len())
	iter := servicePackagesField.MapRange()
	for iter.Next() {
		pkg := iter.Value().Interface().(conns.ServicePackage)
		packages = append(packages, pkg)
	}

	fmt.Printf("Successfully extracted %d service packages via reflection\n", len(packages))
	return packages, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <output-file.json>\n", os.Args[0])
		os.Exit(1)
	}

	outputFile := os.Args[1]

	fmt.Println("Extracting resource schemas from Terraform AWS Provider...")
	fmt.Println("Note: Using reflection to access internal service packages")

	ctx := context.Background()

	// Get service packages via reflection
	servicePackages, err := getServicePackagesViaReflection(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting service packages via reflection: %v\n", err)
		os.Exit(1)
	}

	// Collect all resources from service packages
	resourceMap := make(map[string]*schema.Resource)

	for _, sp := range servicePackages {
		for _, res := range sp.SDKResources(ctx) {
			// Call the factory function to get the unwrapped resource
			resourceMap[res.TypeName] = res.Factory()
		}
	}

	resources := make([]ResourceInfo, 0, len(resourceMap))

	// Get sorted list of resource names for consistent output
	resourceNames := make([]string, 0, len(resourceMap))
	for name := range resourceMap {
		resourceNames = append(resourceNames, name)
	}
	sort.Strings(resourceNames)

	for _, resourceName := range resourceNames {

		resource := resourceMap[resourceName]

		fmt.Printf("%+v\n", resource)
		info := ResourceInfo{
			ResourceType:     resourceName,
			DeprecationMsg:   resource.DeprecationMessage,
			Description:      resource.Description,
			HasImporter:      resource.Importer != nil,
			HasCustomizeDiff: resource.CustomizeDiff != nil,
		}

		if resource.Schema != nil {
			info.Schema = extractSchema(resource.Schema)
		} else if resource.SchemaFunc != nil {
			info.Schema = extractSchema(resource.SchemaFunc())
		}

		// Extract CRUD function names - preserve all variants
		info.CreateWithoutTimeout = getFunctionName(resource.CreateWithoutTimeout)
		info.CreateContext = getFunctionName(resource.CreateContext)
		info.Create = getFunctionName(resource.Create)
		info.ReadWithoutTimeout = getFunctionName(resource.ReadWithoutTimeout)
		info.ReadContext = getFunctionName(resource.ReadContext)
		info.Read = getFunctionName(resource.Read)
		info.UpdateWithoutTimeout = getFunctionName(resource.UpdateWithoutTimeout)
		info.UpdateContext = getFunctionName(resource.UpdateContext)
		info.Update = getFunctionName(resource.Update)
		info.DeleteWithoutTimeout = getFunctionName(resource.DeleteWithoutTimeout)
		info.DeleteContext = getFunctionName(resource.DeleteContext)
		info.Delete = getFunctionName(resource.Delete)

		// Extract timeouts
		if resource.Timeouts != nil {
			info.Timeouts = &TimeoutsInfo{}
			if resource.Timeouts.Create != nil {
				info.Timeouts.Create = resource.Timeouts.Create.String()
			}
			if resource.Timeouts.Read != nil {
				info.Timeouts.Read = resource.Timeouts.Read.String()
			}
			if resource.Timeouts.Update != nil {
				info.Timeouts.Update = resource.Timeouts.Update.String()
			}
			if resource.Timeouts.Delete != nil {
				info.Timeouts.Delete = resource.Timeouts.Delete.String()
			}
		}

		resources = append(resources, info)
		fmt.Printf("  Extracted: %s\n", resourceName)
	}

	// Write to JSON file
	file, err := os.Create(outputFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating output file: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(resources); err != nil {
		fmt.Fprintf(os.Stderr, "Error encoding JSON: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\nSuccessfully extracted %d resources to %s\n", len(resources), outputFile)
}

func getFunctionName(i interface{}) string {
	if i == nil {
		return ""
	}
	fullName := runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
	return fullName
}

func extractSchema(schemaMap map[string]*schema.Schema) map[string]SchemaInfo {
	result := make(map[string]SchemaInfo)

	for key, s := range schemaMap {
		info := SchemaInfo{
			Type:          getSchemaType(s.Type),
			Optional:      s.Optional,
			Required:      s.Required,
			Computed:      s.Computed,
			ForceNew:      s.ForceNew,
			Description:   s.Description,
			Deprecated:    s.Deprecated,
			ConflictsWith: s.ConflictsWith,
			ExactlyOneOf:  s.ExactlyOneOf,
			AtLeastOneOf:  s.AtLeastOneOf,
			RequiredWith:  s.RequiredWith,
			Sensitive:     s.Sensitive,
			MaxItems:      s.MaxItems,
			MinItems:      s.MinItems,
		}

		// Handle default value
		if s.Default != nil {
			info.Default = s.Default
		}

		// Handle Elem (nested schema or type)
		if s.Elem != nil {
			switch elem := s.Elem.(type) {
			case *schema.Resource:
				// Nested resource schema
				info.Elem = map[string]interface{}{
					"type":   "resource",
					"schema": extractSchema(elem.Schema),
				}
			case *schema.Schema:
				// Simple type
				info.Elem = map[string]interface{}{
					"type": getSchemaType(elem.Type),
				}
			case schema.ValueType:
				// Direct value type
				info.Elem = map[string]interface{}{
					"type": getSchemaType(elem),
				}
			}
		}

		result[key] = info
	}

	return result
}

func getSchemaType(t schema.ValueType) string {
	switch t {
	case schema.TypeBool:
		return "bool"
	case schema.TypeInt:
		return "int"
	case schema.TypeFloat:
		return "float"
	case schema.TypeString:
		return "string"
	case schema.TypeList:
		return "list"
	case schema.TypeMap:
		return "map"
	case schema.TypeSet:
		return "set"
	default:
		return "unknown"
	}
}
