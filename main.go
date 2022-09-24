package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/fatih/structs"
	"github.com/gobuffalo/flect"
	flag "github.com/spf13/pflag"
	openvizuiv1alpha1 "go.openviz.dev/grafana-tools/apis/ui/v1alpha1"
	"gomodules.xyz/encoding/json"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
	kmapi "kmodules.xyz/client-go/api/v1"
	metav1alpha1 "kmodules.xyz/resource-metadata/apis/meta/v1alpha1"
	"kmodules.xyz/resource-metadata/pkg/tableconvertor"
	kuebdbuiv1alpha1 "kubedb.dev/apimachinery/apis/ui/v1alpha1"
	kubev1alpha1 "kubeops.dev/ui-server/apis/ui/v1alpha1"
	"sigs.k8s.io/yaml"
	stashuiv1alpha1 "stash.appscode.dev/apimachinery/apis/ui/v1alpha1"
)

var buf bytes.Buffer

func main() {
	dir := flag.String("dir", os.ExpandEnv("$HOME/go/src/kmodules.xyz/resource-metadata/hub/resourcetabledefinitions"), "Path to directory where generated table definitions will be written")
	flag.Parse()

	scheme := runtime.NewScheme()
	_ = kubev1alpha1.AddToScheme(scheme)
	_ = kuebdbuiv1alpha1.AddToScheme(scheme)
	_ = stashuiv1alpha1.AddToScheme(scheme)
	_ = openvizuiv1alpha1.AddToScheme(scheme)
	GenerateForScheme(*dir, scheme)
}

func GenerateForScheme(dir string, scheme *runtime.Scheme) {
	for gvk, typ := range scheme.AllKnownTypes() {
		if strings.HasSuffix(gvk.Kind, "List") || IsOfficialType(gvk.Group) {
			continue
		}
		Generate(dir, gvk, reflect.New(typ).Elem().Interface())
	}
}

func Generate(dir string, gvk schema.GroupVersionKind, s interface{}) {
	f, ok := structs.New(s).FieldOk("Spec")
	if !ok {
		klog.Infof("skipping %s, since no Spec field found", reflect.TypeOf(s))
		return
	}

	v := f.Value()
	prefix := ".spec"
	subfield := ""

	if len(f.Fields()) == 1 && f.Fields()[0].Kind() == reflect.Slice {
		f := f.Fields()[0]
		tag, _, exists := json.ParseTag(f.Tag("json"))
		if !exists {
			klog.Infof("skipping %s, since no Spec.%s has no json tag", reflect.TypeOf(s), f.Name())
			return
		}
		underlyingType := reflect.TypeOf(f.Value()).Elem()
		v = reflect.New(underlyingType).Elem().Interface()
		subfield = tag
		prefix = ".spec." + subfield
	}
	usingSpec := prefix == ".spec"

	rid := kmapi.ResourceID{
		Group:   gvk.Group,
		Version: gvk.Version,
		Name:    flect.Pluralize(strings.ToLower(gvk.Kind)),
		Kind:    gvk.Kind,
		Scope:   kmapi.NamespaceScoped,
	}
	table := metav1alpha1.ResourceTableDefinition{
		TypeMeta: metav1.TypeMeta{
			Kind:       metav1alpha1.ResourceKindResourceTableDefinition,
			APIVersion: metav1alpha1.SchemeGroupVersion.String(),
		},
	}

	var filename string
	if usingSpec {
		table.Name = strings.ToLower(GetName(rid.GroupVersionResource()))
		table.Labels = map[string]string{
			"k8s.io/group":    rid.Group,
			"k8s.io/kind":     rid.Kind,
			"k8s.io/resource": rid.Name,
			"k8s.io/version":  rid.Version,
		}
		filename = strings.ToLower(rid.Name + ".yaml")

		table.Spec = metav1alpha1.ResourceTableDefinitionSpec{
			Resource:    &rid,
			DefaultView: usingSpec,
			Columns:     append(tableconvertor.DefaultDetailsColumns(), ListColumns(prefix, v)...),
		}
	} else {
		table.Name = strings.ToLower(GetName(rid.GroupVersionResource()) + "-" + subfield)
		filename = strings.ToLower(rid.Name + "-" + subfield + ".yaml")

		table.Spec = metav1alpha1.ResourceTableDefinitionSpec{
			Resource:    nil,
			DefaultView: false,
			// FieldPath:   prefix,
			Columns: ListColumns("", v),
		}
	}

	data, _ := yaml.Marshal(table)
	klog.Infoln("writing", filepath.Join(rid.Group, rid.Version, filename))

	buf.Reset()
	if !usingSpec {
		buf.WriteString("# using fieldPath: " + prefix)
		buf.WriteRune('\n')
		buf.WriteRune('\n')
	}
	buf.Write(data)

	path := filepath.Join(dir, rid.Group, rid.Version)
	_ = os.MkdirAll(path, 0755)
	_ = os.WriteFile(filepath.Join(path, filename), buf.Bytes(), 0644)
}

func IsOfficialType(group string) bool {
	switch {
	case group == "":
		return true
	case !strings.ContainsRune(group, '.'):
		return true
	case group == "k8s.io" || strings.HasSuffix(group, ".k8s.io"):
		return true
	case group == "kubernetes.io" || strings.HasSuffix(group, ".kubernetes.io"):
		return true
	case group == "x-k8s.io" || strings.HasSuffix(group, ".x-k8s.io"):
		return true
	default:
		return false
	}
}

func GetName(gvr schema.GroupVersionResource) string {
	if gvr.Group == "" && gvr.Version == "v1" {
		return fmt.Sprintf("core-v1-%s", gvr.Resource)
	}
	return fmt.Sprintf("%s-%s-%s", gvr.Group, gvr.Version, gvr.Resource)
}

func ListColumns(prefix string, v interface{}) []metav1alpha1.ResourceColumnDefinition {
	columns := make([]metav1alpha1.ResourceColumnDefinition, 0)
	for _, f := range structs.Fields(v) {
		if !f.IsExported() {
			continue
		}

		tag, _, exists := json.ParseTag(f.Tag("json"))
		if !exists {
			continue
		}
		fullTag := prefix
		if tag != "" {
			fullTag += "." + tag
		}

		colTitle := flect.Titleize(f.Name())
		if strings.HasSuffix(f.Name(), "Byte") {
			colTitle = flect.Titleize(strings.TrimSuffix(f.Name(), "Byte")) + " (bytes)"
		} else if strings.HasSuffix(f.Name(), "Bytes") {
			colTitle = flect.Titleize(strings.TrimSuffix(f.Name(), "Bytes")) + " (bytes)"
		} else if strings.HasSuffix(f.Name(), "MilliSeconds") {
			colTitle = flect.Titleize(strings.TrimSuffix(f.Name(), "MilliSeconds")) + " (msec)"
		} else if strings.HasSuffix(f.Name(), "MicroSeconds") {
			colTitle = flect.Titleize(strings.TrimSuffix(f.Name(), "MicroSeconds")) + " (µsec)"
		} else if strings.HasSuffix(f.Name(), "Seconds") {
			colTitle = flect.Titleize(strings.TrimSuffix(f.Name(), "Seconds")) + " (sec)"
		} else if strings.HasSuffix(f.Name(), "PercentAsNumber") {
			colTitle = flect.Titleize(strings.TrimSuffix(f.Name(), "PercentAsNumber")) + " (%)"
		} else if strings.HasSuffix(f.Name(), "Percentage") {
			colTitle = flect.Titleize(strings.TrimSuffix(f.Name(), "Percentage")) + " (%)"
		}

		t, v := f.Kind(), f.Value()
		if IsTime(v) {
			columns = append(columns, metav1alpha1.ResourceColumnDefinition{
				Name:         colTitle,
				Type:         "date",
				Format:       "",
				Description:  "",
				Priority:     int32(metav1alpha1.Field | metav1alpha1.List),
				PathTemplate: fmt.Sprintf("{{ %s }}", fullTag),
				Sort: &metav1alpha1.SortDefinition{
					Enable:   true,
					Template: fmt.Sprintf(`{{ %s | toDate "2006-01-02T15:04:05Z07:00" | unixEpoch }}`, fullTag),
					Type:     "integer",
					Format:   "",
				},
				Link:  nil,
				Shape: "",
				Icon:  nil,
				Color: nil,
			})
			continue
		}

		if t == reflect.Ptr || t == reflect.Slice || t == reflect.Array {
			underlyingType := reflect.TypeOf(v).Elem()
			t = underlyingType.Kind()
			v = reflect.New(underlyingType).Elem().Interface()
		}

		if t == reflect.Struct {
			columns = append(columns, ListColumns(fullTag, v)...)
		} else {
			typ, format := kindToType(t)
			col := metav1alpha1.ResourceColumnDefinition{
				Name:         colTitle,
				Type:         typ,
				Format:       format,
				Description:  "",
				Priority:     int32(metav1alpha1.Field | metav1alpha1.List),
				PathTemplate: fmt.Sprintf("{{ %s }}", fullTag),
				Sort:         nil,
				Link:         nil,
				Shape:        "",
				Icon:         nil,
				Color:        nil,
			}
			if IsStringMap(v) || typ == "object" {
				col.PathTemplate = fmt.Sprintf(`{{ %s | toRawJson }}`, fullTag)
			}

			columns = append(columns, col)
		}
	}
	return columns
}

func IsStringMap(v interface{}) bool {
	_, ok := v.(map[string]string)
	return ok
}

func IsTime(v interface{}) bool {
	if _, ok := v.(*metav1.Time); ok {
		return true
	}
	if _, ok := v.(metav1.Time); ok {
		return true
	}
	if _, ok := v.(*time.Time); ok {
		return true
	}
	if _, ok := v.(time.Time); ok {
		return true
	}
	return false
}

func kindToType(k reflect.Kind) (typ string, format string) {
	switch k {
	case reflect.Bool:
		typ = "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		typ = "integer"
	case reflect.Float32:
		typ = "number"
		format = "float"
	case reflect.Float64:
		typ = "number"
		format = "double"
	case reflect.Array, reflect.Slice:
		typ = "array"
	case reflect.Map, reflect.Struct:
		typ = "object"
	case reflect.String:
		typ = "string"

	//case reflect.Complex64,
	// reflect.Complex128,
	// reflect.Chan,
	// reflect.Func,
	// reflect.Interface,
	// reflect.Ptr,
	// reflect.UnsafePointer, reflect.Uintptr:
	default:
		panic(fmt.Errorf("unsupported kind %s", k))
	}
	return
}
