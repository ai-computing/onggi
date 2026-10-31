"""Optional publication figure, derived only from a verified E11 summary."""
import argparse
import json
import pathlib


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('summary',type=pathlib.Path)
    parser.add_argument('--out',type=pathlib.Path,required=True)
    args=parser.parse_args()
    data=json.loads(args.summary.read_text())
    rows={(g['stratum'],g['arm']):g for g in data['groups']}
    order=[('empty','store'),('empty','peer'),('one-seed','store'),('one-seed','peer')]
    assert set(rows)==set(order)
    assert all(rows[k]['cohort_completion_s']['n']==5 for k in order)
    import matplotlib
    matplotlib.use('Agg')
    import matplotlib.pyplot as plt
    plt.rcParams.update({'font.size':9,'pdf.fonttype':42,'ps.fonttype':42})
    fig,axes=plt.subplots(1,3,figsize=(10,3.3),layout='constrained')
    colors=['#2878B5','#EE7733']*2
    labels=['Empty\nstore','Empty\npeer','One seed\nstore','One seed\npeer']
    for ax,metric,scale,title in zip(axes,
            ['cohort_completion_s','pack_body_bytes','successful_body_bytes'],
            [1,1000,1000000],['Cohort completion (s)','Origin pack payload (kB)','All origin body payload (MB)']):
        values=[rows[k][metric]['median']/scale for k in order]
        low=[(rows[k][metric]['median']-rows[k][metric]['q1'])/scale for k in order]
        high=[(rows[k][metric]['q3']-rows[k][metric]['median'])/scale for k in order]
        bars=ax.bar(range(4),values,color=colors,yerr=[low,high],capsize=3,width=.65)
        ax.set_xticks(range(4),labels)
        ax.set_title(title)
        ax.set_ylim(0,max(v+h for v,h in zip(values,high))*1.2+.01)
        ax.spines[['top','right']].set_visible(False)
        ax.set_axisbelow(True)
        ax.grid(axis='y',alpha=.2)
        for bar,v,h in zip(bars,values,high):
            ax.annotate(f'{v:.2f}',(bar.get_x()+bar.get_width()/2,v+h),xytext=(0,5),
                        textcoords='offset points',ha='center',fontsize=8)
    fig.suptitle('Three-node file switch: median and Q1–Q3, n=5 per condition',fontsize=11)
    args.out.mkdir(parents=True,exist_ok=True)
    for ext in ['pdf','svg','png']:
        fig.savefig(args.out/('e11-cohort.'+ext),dpi=200)
    plt.close(fig)


if __name__=='__main__':
    main()
