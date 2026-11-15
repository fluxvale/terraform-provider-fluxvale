package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
)

func domainDelegationSchema(ctx context.Context, t *testing.T) schema.Schema {
	t.Helper()
	d := NewDomainDelegationDataSource().(*domainDelegationDataSource)
	resp := &datasource.SchemaResponse{}
	d.Schema(ctx, datasource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	return resp.Schema
}

func TestDomainDelegationDataSourceReadsThePreview(t *testing.T) {
	ctx := context.Background()
	sch := domainDelegationSchema(ctx, t)

	f := newFakeAPI(t)
	f.domainDelegationDoc = `{"domain": "apps.customer.com",
		"dns_records": [{"name": "_acme-challenge.apps.customer.com", "type": "CNAME",
			"value": "0123456789abcdef0123456789abcdef.acme.fluxvale.app"}]}`

	d := NewDomainDelegationDataSource().(*domainDelegationDataSource)
	cfgResp := &datasource.ConfigureResponse{}
	d.Configure(ctx, datasource.ConfigureRequest{ProviderData: f.testClient()}, cfgResp)
	assertNoErrors(t, &cfgResp.Diagnostics)

	resp := &datasource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	d.Read(ctx, datasource.ReadRequest{
		Config: nullConfig(ctx, t, sch, map[string]string{"domain": "apps.customer.com"}),
	}, resp)
	assertNoErrors(t, &resp.Diagnostics)

	var got domainDelegationModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if got.Domain.ValueString() != "apps.customer.com" {
		t.Errorf("domain = %q, want the server's canonical form", got.Domain.ValueString())
	}

	var records []delegationRecordModel
	if diags := got.DNSRecords.ElementsAs(ctx, &records, false); diags.HasError() {
		t.Fatalf("dns_records: %v", diags)
	}
	if len(records) != 1 {
		t.Fatalf("dns_records = %d, want 1", len(records))
	}
	if records[0].Name.ValueString() != "_acme-challenge.apps.customer.com" ||
		records[0].Type.ValueString() != "CNAME" ||
		records[0].Value.ValueString() != "0123456789abcdef0123456789abcdef.acme.fluxvale.app" {
		t.Errorf("record = %+v, want the preview's challenge CNAME", records[0])
	}

	req := findOne(t, f.all(), "GET", "/api/v1/domain-delegations/apps.customer.com")
	if req.path == "" {
		t.Error("the preview route was not called")
	}
}

func TestDomainDelegationDataSourceSurfacesARefusal(t *testing.T) {
	ctx := context.Background()
	sch := domainDelegationSchema(ctx, t)

	f := newFakeAPI(t)
	f.domainDelegationStatus = 400
	f.domainDelegationDoc = `{"errors": [{"title": "InvalidArgument", "detail": "domain is not a valid domain name"}]}`

	d := NewDomainDelegationDataSource().(*domainDelegationDataSource)
	cfgResp := &datasource.ConfigureResponse{}
	d.Configure(ctx, datasource.ConfigureRequest{ProviderData: f.testClient()}, cfgResp)
	assertNoErrors(t, &cfgResp.Diagnostics)

	resp := &datasource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	d.Read(ctx, datasource.ReadRequest{
		Config: nullConfig(ctx, t, sch, map[string]string{"domain": "not a domain"}),
	}, resp)
	assertError(t, &resp.Diagnostics, "Unable to read the domain delegation", "not a valid domain")
}

func TestDomainDelegationSchemaContract(t *testing.T) {
	ctx := context.Background()
	sch := domainDelegationSchema(ctx, t)

	domain, ok := sch.Attributes["domain"]
	if !ok || !domain.IsRequired() {
		t.Fatal("domain is not Required")
	}
	records, ok := sch.Attributes["dns_records"]
	if !ok || !records.IsComputed() {
		t.Fatal("dns_records is not Computed")
	}
}

func TestDomainDelegationDataSourceConfigureRejectsForeignData(t *testing.T) {
	d := NewDomainDelegationDataSource().(*domainDelegationDataSource)
	resp := &datasource.ConfigureResponse{}
	d.Configure(context.Background(), datasource.ConfigureRequest{ProviderData: "not-a-client"}, resp)
	assertError(t, &resp.Diagnostics, "Unexpected provider data type")
}

func TestDomainDelegationDataSourceReadRequiresConfigure(t *testing.T) {
	ctx := context.Background()
	sch := domainDelegationSchema(ctx, t)

	d := NewDomainDelegationDataSource().(*domainDelegationDataSource)
	resp := &datasource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	d.Read(ctx, datasource.ReadRequest{
		Config: nullConfig(ctx, t, sch, map[string]string{"domain": "apps.customer.com"}),
	}, resp)
	assertError(t, &resp.Diagnostics, "Provider not configured")
}

func TestStringListValueNilStaysNull(t *testing.T) {
	got := stringListValue(nil)
	if !got.IsNull() {
		t.Fatalf("stringListValue(nil) = %v, want null", got)
	}
}
