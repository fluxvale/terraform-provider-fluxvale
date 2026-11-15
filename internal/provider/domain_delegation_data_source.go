package provider

import (
	"context"
	"fmt"

	"github.com/fluxvale/terraform-provider-fluxvale/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSource = (*domainDelegationDataSource)(nil)

// domainDelegationDataSource is fluxvale_domain_delegation — the
// pre-registration DNS delegation preview (fluxvale/fluxvale#783). It exists because a
// computed attribute on fluxvale_custom_domain cannot be consumed by a
// DNS record in the same apply: Terraform does not publish a resource's
// computed values until its create returns, and the domain's create
// waits for the very DNS-01 challenge the record answers. This data
// source is row-free, so the graph is acyclic and one apply works.
type domainDelegationDataSource struct {
	pd *providerData
}

// NewDomainDelegationDataSource returns the data-source factory
// provider.go registers.
func NewDomainDelegationDataSource() datasource.DataSource {
	return &domainDelegationDataSource{}
}

func (d *domainDelegationDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_domain_delegation"
}

type domainDelegationModel struct {
	Domain     types.String `tfsdk:"domain"`
	DNSRecords types.List   `tfsdk:"dns_records"`
}

var delegationRecordAttrTypes = map[string]attr.Type{
	"name":  types.StringType,
	"type":  types.StringType,
	"value": types.StringType,
}

func (d *domainDelegationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The DNS delegation records a custom domain needs BEFORE it is registered. " +
			"The private-access lane needs exactly one `_acme-challenge` CNAME; there is no host record. " +
			"Use this data source to create the record, then `fluxvale_custom_domain` with " +
			"`wait_for_verification = true` in the same apply — never the resource's computed mirror, " +
			"which cannot be published until create returns (and create is waiting for the record).",
		Attributes: map[string]schema.Attribute{
			"domain": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The domain you will register — e.g. `apps.example.com`. The platform canonicalizes " +
					"(trim + downcase) the same way registration does and refuses an invalid name or FluxVale's own " +
					"platform domain.",
			},
			"dns_records": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Computed:    true,
							Description: "The fully-qualified record name (for example `_acme-challenge.apps.example.com`).",
						},
						"type": schema.StringAttribute{
							Computed:    true,
							Description: "The DNS record type — `CNAME` today.",
						},
						"value": schema.StringAttribute{
							Computed:    true,
							Description: "The record target. Bind a DNS provider's `content` to this value; for Cloudflare the record must be DNS-only (`proxied = false`).",
						},
					},
				},
			},
		},
	}
}

func (d *domainDelegationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Got %T, want *providerData. Please report this bug.", req.ProviderData),
		)
		return
	}
	d.pd = pd
}

func (d *domainDelegationDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.pd == nil || d.pd.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The provider was not configured before this data source was used — please report this bug.",
		)
		return
	}

	var data domainDelegationModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	delegation, err := d.pd.client.GetDomainDelegation(ctx, data.Domain.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(
			"Unable to read the domain delegation",
			fmt.Sprintf("domain %q: %v", data.Domain.ValueString(), err),
		)
		return
	}

	data.Domain = types.StringValue(delegation.Domain)
	data.DNSRecords = delegationRecordsList(ctx, delegation.DNSRecords, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func delegationRecordsList(ctx context.Context, records []client.DNSRecord, diags *diag.Diagnostics) types.List {
	elems := make([]delegationRecordModel, 0, len(records))
	for _, record := range records {
		elems = append(elems, delegationRecordModel{
			Name:  types.StringValue(record.Name),
			Type:  types.StringValue(record.Type),
			Value: types.StringValue(record.Value),
		})
	}

	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: delegationRecordAttrTypes}, elems)
	diags.Append(d...)
	return list
}

type delegationRecordModel struct {
	Name  types.String `tfsdk:"name"`
	Type  types.String `tfsdk:"type"`
	Value types.String `tfsdk:"value"`
}
