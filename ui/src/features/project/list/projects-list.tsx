import { useQuery } from '@connectrpc/connect-query';
import { Empty, Flex, Pagination } from 'antd';
import { useEffect } from 'react';

import { LoadingState } from '@ui/features/common';
import { listProjects } from '@ui/gen/api/service/v1alpha1/service-KargoService_connectquery';

import { useLocalStorage } from '../../../utils/use-local-storage';

import { ProjectItem } from './project-item/project-item';
import { ProjectListFilter } from './project-list-filter';
import * as styles from './projects-list.module.less';

const PAGE_SIZE_KEY = 'projects-page-size';
const PAGE_NUMBER_KEY = 'projects-page-number';
const FILTER_KEY = 'projects-filter';

export const ProjectsList = () => {
  const [pageSize, setPageSize] = useLocalStorage(PAGE_SIZE_KEY, 10);
  const [page, setPage] = useLocalStorage(PAGE_NUMBER_KEY, 1);
  const [filter, setFilter] = useLocalStorage(FILTER_KEY, '');

  const { data, isLoading } = useQuery(listProjects, {
    pageSize: pageSize,
    page: page - 1,
    filter
  });

  useEffect(() => {
    if (data && page > Math.ceil(data.total / pageSize)) {
      setPage(Math.ceil(data.total / pageSize) || 1);
    }
  }, [data, page, pageSize, setPage]);

  const handlePaginationChange = (newPage: number, newPageSize: number) => {
    setPage(newPage);
    setPageSize(newPageSize);
  };

  const handleFilterChange = (newFilter: string) => {
    setFilter(newFilter);
    setPage(1);
  };

  if (isLoading) return <LoadingState />;

  const isEmpty = !data || data.projects.length === 0;
  const projectListFilter = () => <ProjectListFilter onChange={handleFilterChange} init={filter} />;

  if (isEmpty) {
    return (
      <>
        <div className='flex items-center mb-20'>{projectListFilter()}</div>
        <Empty />
      </>
    );
  }

  return (
    <>
      <div className='mb-6'>{projectListFilter()}</div>
      <div className={styles.list}>
        {data.projects.map((proj) => (
          <ProjectItem key={proj?.metadata?.name} project={proj} />
        ))}
      </div>
      <Flex justify='flex-end' className='mt-8'>
        <Pagination
          total={data?.total || 0}
          className='ml-auto flex-shrink-0'
          pageSize={pageSize}
          current={page}
          onChange={handlePaginationChange}
          showSizeChanger
          hideOnSinglePage
        />
      </Flex>
    </>
  );
};
