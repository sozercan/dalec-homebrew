package resolution

import "testing"

func TestProjectV2ForRuntimePreservesFullIdentity(t *testing.T) {
	record := validRecordV2()
	projected, racks, err := ProjectV2ForRuntime(record)
	if err != nil {
		t.Fatal(err)
	}
	if racks["acme/tools/widget"] != "widget" {
		t.Fatalf("racks=%v", racks)
	}
	var widget Node
	for _, node := range projected.Nodes {
		if node.Name == "widget" {
			widget = node
		}
	}
	if widget.FullName != "acme/tools/widget" || widget.UpstreamFormulaID != "" || projected.PolicyVersion != PolicyVersionV2 {
		t.Fatalf("projected widget=%+v policy=%q", widget, projected.PolicyVersion)
	}
}

func TestProjectV2ForRuntimePreservesHistoricalTabDependencyIdentity(t *testing.T) {
	record := validRecordV2()
	historical := RuntimeDependencyV2{
		ID:               "archive/legacy/libfoo",
		HomebrewFullName: "archive/legacy/libfoo",
		Version:          "1.2.3",
		Revision:         4,
		BottleRebuild:    5,
		PkgVersion:       "1.2.3_4",
		DeclaredDirectly: true,
	}
	widget := nodeV2(record, "acme/tools/widget")
	widget.Bottle.Tab.Dependencies = append(widget.Bottle.Tab.Dependencies, historical)

	projected, _, err := ProjectV2ForRuntime(record)
	if err != nil {
		t.Fatal(err)
	}

	var got *RuntimeDependency
	for i := range projected.Nodes {
		if projected.Nodes[i].FullName != "acme/tools/widget" {
			continue
		}
		for j := range projected.Nodes[i].Bottle.Tab.Dependencies {
			dependency := &projected.Nodes[i].Bottle.Tab.Dependencies[j]
			if dependency.FullName == historical.HomebrewFullName.String() {
				got = dependency
				break
			}
		}
	}
	if got == nil {
		t.Fatalf("historical dependency %q was not projected", historical.HomebrewFullName)
	}
	if got.Version != historical.Version || got.Revision != historical.Revision || got.BottleRebuild != historical.BottleRebuild || got.PkgVersion != historical.PkgVersion || got.DeclaredDirectly != historical.DeclaredDirectly {
		t.Fatalf("projected historical dependency=%+v, want %+v", *got, historical)
	}
}
